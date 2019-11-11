package persistence

import (
	"errors"
	"fmt"

	"github.com/gofrs/uuid"
)

func (p *persistenceLayer) GetAccount(accountID string, includeEvents bool, eventsSince string) (AccountResult, error) {
	account, err := p.dal.FindAccount(FindAccountQueryIncludeEvents{
		AccountID: accountID,
		Since:     eventsSince,
	})
	if err != nil {
		return AccountResult{}, fmt.Errorf("persistence: error looking up account data: %w", err)
	}
	result := AccountResult{
		AccountID: account.AccountID,
		Name:      account.Name,
	}

	if includeEvents {
		result.EncryptedPrivateKey = account.EncryptedPrivateKey
	} else {
		key, err := account.WrapPublicKey()
		if err != nil {
			return AccountResult{}, fmt.Errorf("persistence: error wrapping account public key: %v", err)
		}
		result.PublicKey = key
	}

	eventResults := EventsByAccountID{}
	userSecrets := SecretsByUserID{}

	for _, evt := range account.Events {
		eventResults[evt.AccountID] = append(eventResults[evt.AccountID], EventResult{
			UserID:    evt.HashedUserID,
			EventID:   evt.EventID,
			Payload:   evt.Payload,
			AccountID: evt.AccountID,
		})
		if evt.HashedUserID != nil {
			userSecrets[*evt.HashedUserID] = evt.User.EncryptedUserSecret
		}
	}

	if len(eventResults) != 0 {
		result.Events = &eventResults
	}
	if len(userSecrets) != 0 {
		result.UserSecrets = &userSecrets
	}

	return result, nil
}

func (p *persistenceLayer) AssociateUserSecret(accountID, userID, encryptedUserSecret string) error {
	account, err := p.dal.FindAccount(FindAccountQueryByID(accountID))
	if err != nil {
		return fmt.Errorf(`persistence: error looking up account with id "%s": %w`, accountID, err)
	}

	hashedUserID := account.HashUserID(userID)
	user, err := p.dal.FindUser(FindUserQueryByHashedUserID(hashedUserID))
	// there is an issue with the Postgres backend of GORM that disallows inserting
	// primary keys when using `FirstOrCreate`, so we need to do a manual check
	// for existence beforehand.
	if err != nil {
		var notFound ErrUnknownUser
		if !errors.As(err, &notFound) {
			return fmt.Errorf("persistence: error looking up user: %v", err)
		}
	} else {
		// In this branch the following case is covered: a user whose hashed
		// identifier is known, has sent a new user secret to be saved. This means
		// all events previously saved using their identifier cannot be accessed
		// by them anymore as the key that has been used to encrypt the event's payloads
		// is not known to the user anymore. This means all events that are
		// currently associated to the identifier will be migrated to a newly
		// created identifier which will be used to "park" them. It is important
		// to update these event's EventIDs as this means they will be considered
		// "deleted" by clients.
		parkedID, parkedIDErr := uuid.NewV4()
		if parkedIDErr != nil {
			return fmt.Errorf("persistence: error creating identifier for parking events: %v", parkedIDErr)
		}
		parkedHash := account.HashUserID(parkedID.String())

		txn, err := p.dal.Transaction()
		if err != nil {
			return fmt.Errorf("persistence: error creating transaction: %w", err)
		}
		if err := txn.CreateUser(&User{
			HashedUserID:        parkedHash,
			EncryptedUserSecret: user.EncryptedUserSecret,
		}); err != nil {
			txn.Rollback()
			return fmt.Errorf("persistence: error creating user for use as migration target: %w", err)
		}

		if err := txn.DeleteUser(DeleteUserQueryByHashedID(user.HashedUserID)); err != nil {
			txn.Rollback()
			return fmt.Errorf("persistence: error deleting existing user: %v", err)
		}

		// The previous user is now deleted so all orphaned events need to be
		// copied over to the one used for parking the events.
		var idsToDelete []string
		orphanedEvents, err := txn.FindEvents(FindEventsQueryForHashedIDs{
			HashedUserIDs: []string{hashedUserID},
		})
		if err != nil {
			return fmt.Errorf("persistence: error looking up orphaned events: %w", err)
		}
		for _, orphan := range orphanedEvents {
			newID, err := newEventID()
			if err != nil {
				txn.Rollback()
				return fmt.Errorf("persistence: error creating new event id: %w", err)
			}

			if err := txn.CreateEvent(&Event{
				EventID:      newID,
				AccountID:    orphan.AccountID,
				HashedUserID: &parkedHash,
				Payload:      orphan.Payload,
			}); err != nil {
				return fmt.Errorf("persistence: error migrating an existing event: %w", err)
			}
			idsToDelete = append(idsToDelete, orphan.EventID)
		}
		if _, err := txn.DeleteEvents(DeleteEventsQueryByEventIDs(idsToDelete)); err != nil {
			txn.Rollback()
			return fmt.Errorf("persistence: error deleting orphaned events: %w", err)
		}
		if err := txn.Commit(); err != nil {
			return fmt.Errorf("persistence: error committing transaction: %w", err)
		}
	}

	if err := p.dal.CreateUser(&User{
		EncryptedUserSecret: encryptedUserSecret,
		HashedUserID:        hashedUserID,
	}); err != nil {
		return fmt.Errorf("persistence: error creating user: %w", err)
	}
	return nil
}
