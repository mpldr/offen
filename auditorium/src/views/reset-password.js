/** @jsx h */
const { h } = require('preact')
const { connect } = require('react-redux')

const authentication = require('./../action-creators/authentication')
const withLayout = require('./components/_shared/with-layout')
const Form = require('./components/reset-password/form')

const ResetPasswordView = (props) => {
  return (
    <div class='w-100 mt4 mb2 br0 br2-ns'>
      <Form
        onResetPassword={props.handleResetPassword}
      />
    </div>
  )
}

const mapDispatchToProps = {
  handleReset: authentication.resetPassword
}

module.exports = connect(null, mapDispatchToProps)(
  withLayout()(
    ResetPasswordView
  )
)
