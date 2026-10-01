package business

import (
	"encoding/json"
	"strings"
	"testing"
)

// A realistic key, with a PEM body that is structurally right and cryptographically
// meaningless. Nothing here is a secret.
const fakeServiceAccount = `{
  "type": "service_account",
  "project_id": "onecamp-test",
  "private_key_id": "0000",
  "private_key": "-----BEGIN PRIVATE KEY-----\nQUJD\n-----END PRIVATE KEY-----\n",
  "client_email": "push@onecamp-test.iam.gserviceaccount.com",
  "client_id": "1",
  "token_uri": "https://oauth2.googleapis.com/token"
}`

func TestValidateAcceptsAServiceAccountKey(t *testing.T) {
	project, email, err := ValidateFirebaseCredential(fakeServiceAccount)
	if err != nil {
		t.Fatalf("a well-formed key was rejected: %v", err)
	}
	if project != "onecamp-test" || !strings.HasPrefix(email, "push@") {
		t.Fatalf("identity not extracted: %q %q", project, email)
	}
}

// Every one of these was stored happily before validation existed, and Firebase
// only reports the problem at initialisation. That is how push is off for
// months without anybody knowing.
func TestValidateRejectsWhatIsNotAServiceAccountKey(t *testing.T) {
	cases := map[string]string{
		"empty":                                  "",
		"not JSON":                               "paste your key here",
		"an OAuth client, not a service account": `{"type":"authorized_user","project_id":"x"}`,
		"the web app config by mistake":          `{"apiKey":"AIza...","projectId":"x"}`,
		"missing the private key":                `{"type":"service_account","project_id":"x","client_email":"a@b"}`,
		"missing the project":                    `{"type":"service_account","private_key":"-----BEGIN PRIVATE KEY-----","client_email":"a@b"}`,
		"a truncated paste":                      `{"type":"service_account","project_id":"x","client_email":"a@b","private_key":"-----BEGIN PRI"}`,
	}
	for name, raw := range cases {
		if _, _, err := ValidateFirebaseCredential(raw); err == nil {
			t.Errorf("accepted %s", name)
		}
	}
}

func TestValidateRefusesSomethingTooLargeToBeAKey(t *testing.T) {
	if _, _, err := ValidateFirebaseCredential(strings.Repeat("a", maxCredentialBytes+1)); err == nil {
		t.Fatal("accepted a document far larger than any service-account key")
	}
}

// The status object is rendered in the browser and travels through logs and
// screenshots. It must carry enough to identify the key and nothing that could
// be used as one.
func TestStatusNeverCarriesTheSecret(t *testing.T) {
	var sa serviceAccount
	if err := json.Unmarshal([]byte(fakeServiceAccount), &sa); err != nil {
		t.Fatal(err)
	}
	st := PushCredentialStatus{
		Configured:  true,
		Source:      "settings",
		ProjectID:   sa.ProjectID,
		ClientEmail: sa.ClientEmail,
		Active:      true,
	}
	encoded, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	body := string(encoded)
	for _, forbidden := range []string{"private_key", "PRIVATE KEY", "QUJD"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("the status payload leaks %q: %s", forbidden, body)
		}
	}
	// And it must still be useful, or an admin cannot tell which key is loaded.
	if !strings.Contains(body, "onecamp-test") {
		t.Fatalf("the status payload does not identify the project: %s", body)
	}
}

// The struct itself must not grow a field that serialises the key, which is the
// way this leaks later: somebody adds one for debugging and it reaches the wire.
func TestStatusStructHasNoCredentialField(t *testing.T) {
	encoded, _ := json.Marshal(PushCredentialStatus{})
	var fields map[string]any
	_ = json.Unmarshal(encoded, &fields)
	for name := range fields {
		if strings.Contains(name, "key") || strings.Contains(name, "credential") || strings.Contains(name, "secret") {
			t.Fatalf("PushCredentialStatus exposes %q", name)
		}
	}
}
