// Copyright 2020-2021 - Offen Authors <hioffen@posteo.de>
// SPDX-License-Identifier: Apache-2.0

package router

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/NYTimes/gziphandler"
	"github.com/felixge/httpsnoop"
	"github.com/gin-contrib/location"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/securecookie"
	"github.com/microcosm-cc/bluemonday"
	"github.com/offen/offen/server/config"
	"github.com/offen/offen/server/mailer"
	"github.com/offen/offen/server/persistence"
	ratelimiter "github.com/offen/offen/server/ratelimiter"
	"github.com/patrickmn/go-cache"
	"github.com/sirupsen/logrus"
	"mpldr.codes/oidc"
)

type router struct {
	db           persistence.Service
	mailer       mailer.Mailer
	fs           http.FileSystem
	logger       *logrus.Logger
	cookieSigner *securecookie.SecureCookie
	template     *template.Template
	emails       *template.Template
	config       *config.Config
	sanitizer    *bluemonday.Policy
	limiter      ratelimiter.Throttler
	cache        *cache.Cache
	oidc         *oidc.Configuration
}

func (rt *router) getLimiter() ratelimiter.Throttler {
	if rt.limiter == nil {
		if rt.config != nil && rt.config.Server.ReverseProxy {
			rt.limiter = ratelimiter.NewNoopRateLimiter()
		} else {
			rt.limiter = ratelimiter.New(time.Second*30, cache.New(time.Minute, time.Minute*2))
		}
	}
	return rt.limiter
}

func (rt *router) getCache() *cache.Cache {
	if rt.cache == nil {
		rt.cache = cache.New(cache.NoExpiration, time.Minute)
	}
	return rt.cache
}

func (rt *router) logError(err error, message string) {
	sanitizedErrorMessage := strings.ReplaceAll(err.Error(), "\n", " ")
	if rt.logger != nil {
		rt.logger.WithError(errors.New(sanitizedErrorMessage)).Error(message)
	}
}

const (
	cookieKey               = "user"
	optinKey                = "consent"
	optinValue              = "allow"
	authKey                 = "auth"
	contextKeyCookie        = "contextKeyCookie"
	contextKeyAuth          = "contextKeyAuth"
	contextKeySecureContext = "contextKeySecure"
)

func (rt *router) userCookie(userID string, secure bool) *http.Cookie {
	sameSite := http.SameSiteNoneMode
	if !secure {
		sameSite = http.SameSiteLaxMode
	}

	c := &http.Cookie{
		Name:     cookieKey,
		Value:    userID,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
		Path:     "/api",
	}
	if userID != "" {
		c.Expires = time.Now().Add(config.EventRetention)
	}
	return c
}

func (rt *router) authCookie(userID string, secure bool) (*http.Cookie, error) {
	c := http.Cookie{
		Name:     authKey,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		Path:     "/api",
	}
	if userID == "" {
		c.Expires = time.Unix(0, 0)
	} else {
		value, err := rt.cookieSigner.MaxAge(24*60*60).Encode(authKey, userID)
		if err != nil {
			return nil, err
		}
		c.Value = value
	}
	return &c, nil
}

// Config adds a configuration value to the router
type Config func(*router)

// WithDatabase sets the database the router will use
func WithDatabase(db persistence.Service) Config {
	return func(r *router) {
		r.db = db
	}
}

// WithLogger sets the logger the router will use
func WithLogger(l *logrus.Logger) Config {
	return func(r *router) {
		r.logger = l
	}
}

// WithTemplate ensures the router is using the given template object
// for rendering dynamic HTML output.
func WithTemplate(t *template.Template) Config {
	return func(r *router) {
		r.template = t
	}
}

// WithEmails ensures the router is using the given template object
// for rendering email output.
func WithEmails(t *template.Template) Config {
	return func(r *router) {
		r.emails = t
	}
}

// WithConfig attaches the given runtime config to the router.
func WithConfig(c *config.Config) Config {
	return func(r *router) {
		r.config = c
	}
}

// WithFS attaches a filesystem for serving static assets
func WithFS(fs http.FileSystem) Config {
	return func(r *router) {
		r.fs = fs
	}
}

// WithMailer attaches a mailer for sending transactional email
func WithMailer(m mailer.Mailer) Config {
	return func(r *router) {
		r.mailer = m
	}
}

func WithOIDC(oidc *oidc.Configuration) Config {
	return func(r *router) {
		r.oidc = oidc
	}
}

// New creates a new application router that reads and writes data
// to the given database implementation. In the context of the application
// this expects to be the only top level router in charge of handling all
// incoming HTTP requests.
func New(opts ...Config) http.Handler {
	rt := router{}
	for _, opt := range opts {
		opt(&rt)
	}

	rt.sanitizer = bluemonday.StrictPolicy()
	rt.cookieSigner = securecookie.New(rt.config.Secret.Bytes(), nil)

	optin := optinMiddleware(optinKey, optinValue)
	userCookie := userCookieMiddleware(cookieKey, contextKeyCookie)
	accountAuth := rt.accountUserMiddleware(authKey, contextKeyAuth)
	noStore := headerMiddleware(map[string]func() string{
		"Cache-Control": func() string {
			return "no-store"
		},
	})

	csp := headerMiddleware(map[string]func() string{
		"Content-Security-Policy": func() string {
			return defaultCSP
		},
	})
	etag := etagMiddleware()

	if !rt.config.App.Development {
		gin.SetMode(gin.ReleaseMode)
	}

	app := gin.New()
	app.SetHTMLTemplate(rt.template)
	app.Use(
		gin.Recovery(),
		location.Default(),
		secureContextMiddleware(contextKeySecureContext, rt.config.App.Development),
	)

	app.Any("/healthz", noStore, rt.getHealth)
	app.GET("/versionz", noStore, rt.getVersion)

	app.GET("/vault", etag, csp, rt.getVault)
	if rt.config.App.DemoAccount != "" {
		app.GET("/intro", etag, csp, rt.getIntro)
	}

	{
		api := app.Group("/api")
		api.Use(noStore)
		api.GET("/exchange", rt.getPublicKey)
		api.POST("/exchange", rt.postUserSecret)

		api.GET("/accounts/:accountID", accountAuth, rt.getAccount)
		api.DELETE("/accounts/:accountID", accountAuth, rt.deleteAccount)
		api.PUT("/accounts/:accountID/account-styles", accountAuth, rt.putAccountStyles)
		api.POST("/accounts", accountAuth, rt.postAccount)

		api.POST("/purge", userCookie, rt.purgeEvents)

		api.GET("/login", accountAuth, rt.getLogin)
		if rt.oidc == nil {
			api.POST("/login", rt.postLogin)
			api.POST("/logout", rt.postLogout)

			api.POST("/change-password", accountAuth, rt.postChangePassword)
			api.POST("/change-email", accountAuth, rt.postChangeEmail)
			api.POST("/forgot-password", rt.postForgotPassword)
			api.POST("/reset-password", rt.postResetPassword)
			api.POST("/share-account/:accountID", accountAuth, rt.postShareAccount)
			api.POST("/share-account", accountAuth, rt.postShareAccount)
			api.POST("/join", rt.postJoin)
		} else {
			api.POST("/login", rt.oauthLogin)
			api.POST("/login/callback", rt.oauthCallback)
			api.POST("/logout", rt.oauthLogout)
		}
		api.GET("/setup", rt.getSetup)
		api.POST("/setup", rt.postSetup)

		api.GET("/events", userCookie, rt.getEvents)
		api.POST("/events", optin, userCookie, rt.postEvents)
	}

	root := gin.New()
	root.SetHTMLTemplate(rt.template)
	root.GET("/*any", etag, csp, rt.getIndex)

	app.Use(staticMiddleware(http.FileServer(rt.fs), root))

	if rt.config.Server.ReverseProxy {
		return app
	}

	withGzip := gziphandler.GzipHandler(app)
	// HTTP logging is only added when the reverse proxy setting is not
	// enabled
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metrics := httpsnoop.CaptureMetrics(withGzip, w, r)
		fmt.Printf(
			"%s %s %s [%s] \"%s %s %s\" %d %s\n",
			"-",
			"-",
			"-",
			time.Now().Format("02/Jan/2006:15:04:05 -0700"),
			r.Method,
			r.RequestURI,
			r.Proto,
			anonymizeStatusCode(metrics.Code),
			"-",
		)
	})
}

// anonymizeStatusCode turns all non-error status codes into http.StatusOK
// in order not to leak information about returning visitors that have opted
// out while still providing information about failing requests
func anonymizeStatusCode(code int) int {
	if http.StatusOK <= code && code < http.StatusBadRequest {
		return http.StatusOK
	}
	return code
}
