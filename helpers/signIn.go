package helpers

// How people may sign in.

import "strings"

// PasswordSignInOff reports whether this workspace has turned off signing in
// and signing up with a password (AUTH_EMAIL_DISABLED=true): people use
// Google, GitHub or single sign-on instead. Admins keep their password as a
// way back in when single sign-on breaks (controllers/Auth EmailLogin).
//
// The sign-in page used to read the switch backwards ("false" turned the
// password off) and nothing else read it at all, so a workspace that turned
// passwords off still took them from anyone who sent one.
func PasswordSignInOff() bool {
	return EnvFlag("AUTH_EMAIL_DISABLED")
}

// TrueClaim reads a claim an identity provider sends as true or false, which
// some send as the string "true" (email_verified, from AWS Cognito among
// others). Pure.
func TrueClaim(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(strings.TrimSpace(t), "true")
	}
	return false
}
