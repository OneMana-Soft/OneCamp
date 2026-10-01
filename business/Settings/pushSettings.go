package business

// Push notifications, configured by an admin rather than mounted by an operator.
//
// The Firebase service-account key used to arrive as a file baked into the
// image by `COPY . .`, which meant a private key shipped inside a distributable
// artefact to every customer. Removing it from the build was correct and left a
// hole: the shipped env still points FIREBASE_CRED_PATH at a file the archive
// does not contain, so push has been silently off ever since, on this
// deployment and on every install that followed the guide.
//
// A credential should reach a service the way every other credential here does:
// pasted once by someone with the authority to hold it, encrypted at rest,
// changeable without a redeploy. That is what this is.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
)

// maxCredentialBytes bounds what an admin can paste. A Google service-account
// key is about 2.3 KB; anything at this size is a mistake or an attack, and
// finding out before decoding beats finding out inside the JSON parser.
const maxCredentialBytes = 64 << 10

// serviceAccount is the subset of a Google service-account key this needs to
// recognise one. Parsed for validation only: the whole document is what gets
// stored, because Firebase reads fields this does not model.
type serviceAccount struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	PrivateKey  string `json:"private_key"`
	ClientEmail string `json:"client_email"`
}

// PushCredentialStatus is what the admin screen may see.
//
// Everything here is safe to render. The private key is deliberately absent and
// there is no endpoint that returns it: a credential that can be read back is a
// credential that leaks through a screenshot, a browser cache or a support
// ticket. An admin who needs a different key pastes a different key.
type PushCredentialStatus struct {
	Configured  bool   `json:"configured"`
	Source      string `json:"source"` // "settings" | "file" | "none"
	ProjectID   string `json:"project_id,omitempty"`
	ClientEmail string `json:"client_email,omitempty"`
	// Active reports whether the messaging client actually loaded, which is a
	// different question from whether a credential is stored: a key can be
	// present and rejected.
	Active bool `json:"active"`
}

// FirebaseCredentialJSON returns the service-account document to initialise
// with, or "" when push is not configured.
//
// Setting first, then the file the environment points at. That order matters
// for an existing deployment: one that still mounts a key keeps working
// untouched, and setting one in the admin screen takes over without an operator
// having to remove anything.
func FirebaseCredentialJSON() string {
	all := loadAll()
	if enc, ok := all[keyFirebaseCred]; ok && enc != "" {
		if dec, err := helpers.DecryptSecret(enc); err == nil && dec != "" {
			return dec
		}
		// A credential that will not decrypt is not the same as no credential.
		// Saying so is the difference between "push is off because nobody set
		// it up" and "push is off because the encryption key changed".
		helpers.MessageLogs.ErrorLog.Println(
			"push: stored Firebase credential cannot be decrypted (usually an encryption key change); re-enter it in admin settings")
		return ""
	}
	return firebaseCredentialFromFile()
}

// firebaseCredentialFromFile reads the legacy mounted credential.
func firebaseCredentialFromFile() string {
	path := strings.TrimSpace(os.Getenv("FIREBASE_CRED_PATH"))
	if path == "" {
		return ""
	}
	body, err := os.ReadFile(path)
	if err != nil {
		// Not an error worth logging on every call: the shipped env names a file
		// most installs do not have, and that is the documented, supported state.
		return ""
	}
	return string(body)
}

// ValidateFirebaseCredential checks that a pasted document is a service-account
// key before it is stored.
//
// Stored-then-discovered is the failure this prevents. Firebase only reports a
// bad credential at initialisation, so without this an admin pastes the wrong
// file, sees "saved", and learns months later that no notification was ever
// delivered. Returns the parsed identity so the caller can show what was
// accepted.
func ValidateFirebaseCredential(raw string) (projectID, clientEmail string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("paste the service-account JSON")
	}
	if len(raw) > maxCredentialBytes {
		return "", "", fmt.Errorf("that is too large to be a service-account key")
	}

	var sa serviceAccount
	if uerr := json.Unmarshal([]byte(raw), &sa); uerr != nil {
		return "", "", fmt.Errorf("that is not valid JSON")
	}
	if sa.Type != "service_account" {
		return "", "", fmt.Errorf(`this JSON is not a service account (its "type" is %q)`, sa.Type)
	}
	for field, value := range map[string]string{
		"project_id":   sa.ProjectID,
		"private_key":  sa.PrivateKey,
		"client_email": sa.ClientEmail,
	} {
		if strings.TrimSpace(value) == "" {
			return "", "", fmt.Errorf("the service-account JSON is missing %s", field)
		}
	}
	// A key downloaded from the Google console carries a PEM block. Catching a
	// truncated paste here beats an opaque failure inside the Firebase SDK.
	if !strings.Contains(sa.PrivateKey, "PRIVATE KEY") {
		return "", "", fmt.Errorf("the private_key does not look like a key; the paste may be truncated")
	}
	return sa.ProjectID, sa.ClientEmail, nil
}

// SetFirebaseCredential validates, encrypts and stores a service-account key.
func SetFirebaseCredential(raw string) (projectID, clientEmail string, err error) {
	projectID, clientEmail, err = ValidateFirebaseCredential(raw)
	if err != nil {
		return "", "", err
	}
	enc, err := helpers.EncryptSecret(strings.TrimSpace(raw))
	if err != nil {
		return "", "", fmt.Errorf("could not encrypt the credential: %w", err)
	}
	if err := configModel.UpsertConfig(keyFirebaseCred, enc); err != nil {
		return "", "", err
	}
	invalidate()
	return projectID, clientEmail, nil
}

// ClearFirebaseCredential removes the stored key, turning push off.
//
// It does not delete the key at Google. An admin who is rotating a compromised
// key must revoke it there as well, and the admin screen says so, because a
// removed setting reads like a solved problem.
func ClearFirebaseCredential() error {
	if err := configModel.UpsertConfig(keyFirebaseCred, ""); err != nil {
		return err
	}
	invalidate()
	return nil
}

// PushStatus describes the current configuration for the admin screen.
func PushStatus(active bool) PushCredentialStatus {
	st := PushCredentialStatus{Active: active, Source: "none"}

	all := loadAll()
	raw := ""
	if enc, ok := all[keyFirebaseCred]; ok && enc != "" {
		if dec, derr := helpers.DecryptSecret(enc); derr == nil {
			raw, st.Source = dec, "settings"
		} else {
			// Stored but unreadable. Reporting it as configured would be a lie
			// and reporting "none" would hide a fixable fault.
			st.Source = "settings"
			return st
		}
	} else if fromFile := firebaseCredentialFromFile(); fromFile != "" {
		raw, st.Source = fromFile, "file"
	}

	if raw == "" {
		return st
	}
	var sa serviceAccount
	if json.Unmarshal([]byte(raw), &sa) == nil {
		st.Configured = true
		st.ProjectID = sa.ProjectID
		st.ClientEmail = sa.ClientEmail
	}
	return st
}
