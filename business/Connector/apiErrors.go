package business

// What went wrong when a connected account (Gmail, Calendar, GitHub) refused a
// call, sorted into what the person can do about it. The inbox turns a problem
// into an HTTP status; the AI tools turn it into a sentence. Both used to carry
// their own copy of this logic, and only one of them knew a revoked token from
// an outage.

import (
	"errors"
	"strings"

	"google.golang.org/api/googleapi"
)

// APIProblem is a refused call, in terms a person can act on.
type APIProblem int

const (
	ProblemNone APIProblem = iota
	// ProblemUnknown is anything else: usually a passing outage.
	ProblemUnknown
	// ProblemExpired means the connection was revoked or expired: reconnect.
	ProblemExpired
	// ProblemPermissions means the connection lacks a permission: reconnect and grant it.
	ProblemPermissions
	// ProblemAPIDisabled means the API is off in the workspace's Google Cloud project: an admin fixes it.
	ProblemAPIDisabled
	ProblemRateLimited
	ProblemNotFound
)

// ErrCredentialUnreadable is a saved connection the server cannot decrypt: it
// was stored under another APP_SECRET_KEK (a changed key, or data restored from
// another install). No retry can fix it; connecting again overwrites it.
var ErrCredentialUnreadable = errors.New("the saved connection cannot be read")

// InputError is a request the person can fix by changing what they sent.
type InputError string

func (e InputError) Error() string { return string(e) }

const (
	// ErrBadHeader is a recipient or subject carrying a line break, which would
	// let it add headers of its own to the message.
	ErrBadHeader    InputError = "an address or subject cannot contain a line break"
	ErrBadThreadID  InputError = "not a conversation id"
	ErrEmptyReply   InputError = "write a reply first"
	ErrNoRecipient  InputError = "could not tell who to reply to"
	ErrNoRecipients InputError = "recipient is required"
)

// ClassifyAPIError sorts a provider error. It prefers the typed Google error
// and falls back to the message for GitHub, token refresh and transport errors.
func ClassifyAPIError(err error) APIProblem {
	if err == nil {
		return ProblemNone
	}
	if errors.Is(err, ErrCredentialUnreadable) {
		return ProblemExpired
	}
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		switch gerr.Code {
		case 401:
			return ProblemExpired
		case 403:
			if serviceDisabled(gerr.Message) {
				return ProblemAPIDisabled
			}
			for _, e := range gerr.Errors {
				if e.Reason == "accessNotConfigured" || serviceDisabled(e.Message) {
					return ProblemAPIDisabled
				}
				if e.Reason == "rateLimitExceeded" || e.Reason == "userRateLimitExceeded" {
					return ProblemRateLimited
				}
			}
			return ProblemPermissions
		case 404:
			return ProblemNotFound
		case 429:
			return ProblemRateLimited
		}
	}

	msg := err.Error()
	switch {
	case serviceDisabled(msg):
		return ProblemAPIDisabled
	case strings.Contains(msg, "invalid_grant") || strings.Contains(msg, "expired or revoked"),
		strings.Contains(msg, "401") || strings.Contains(msg, "Unauthorized"):
		return ProblemExpired
	case strings.Contains(msg, "403") || strings.Contains(msg, "Forbidden") || strings.Contains(msg, "insufficient"):
		return ProblemPermissions
	case strings.Contains(msg, "429") || strings.Contains(msg, "rate limit"):
		return ProblemRateLimited
	case strings.Contains(msg, "404") || strings.Contains(msg, "Not Found"):
		return ProblemNotFound
	}
	return ProblemUnknown
}

func serviceDisabled(msg string) bool {
	return strings.Contains(msg, "SERVICE_DISABLED") || strings.Contains(msg, "has not been used in project")
}
