package main

import (
	"errors"
	"fmt"
	"os"
)

// Fixed text is shared by JSON display_message and the interactive console.
// Neither the step nor the message is derived from helper or service output.
var authStepMessages = map[string]string{
	"checking_session":   "Checking saved sign-in...",
	"validating_session": "Validating sign-in...",
	"refreshing_session": "Renewing sign-in...",
	"saving_session":     "Saving sign-in securely...",
	"authenticated":      "Signed in successfully.",
}

func reportAuthStep(step string) {
	message, known := authStepMessages[step]
	if !known {
		return
	}
	if events != nil {
		events.authStep(step, message)
	} else {
		fmt.Fprintln(os.Stderr, message)
	}
}

func sessionAuthenticationError(err error) error {
	if errors.Is(err, errAuthRejected) {
		return &operationError{code: "authentication_failed", phase: "authenticating", reason: "session_rejected", err: err}
	}
	return operationFailure(err, "authentication_failed", "authenticating")
}
