//go:build integration
// +build integration

package integration_test

// Signing in through the directory asks for the second step.
//
// Two-step was enforced only on the email sign-in, so someone with it on who
// could also sign in through LDAP skipped it there: the directory's password
// alone was a session. This signs in through LDAPLogin with the directory
// stood in for (the harness has no LDAP server) and everything after its
// answer real: the enrolment and the session in Postgres and Redis.
//
// Run: go test -tags=integration ./tests/integration/ -run TestDirectorySignIn -v

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	authBusiness "github.com/akashc777/OneCamp/business/Auth"
	authController "github.com/akashc777/OneCamp/controllers/Auth"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	ldapService "github.com/akashc777/OneCamp/services/LDAP"
	"github.com/akashc777/OneCamp/tests/integration"
)

// totpCode is what an authenticator app shows for secret at a moment (RFC 6238:
// SHA-1, six digits, thirty-second steps).
func totpCode(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatal(err)
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(at.Unix()/30))
	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	o := sum[len(sum)-1] & 0x0f
	v := (uint32(sum[o]&0x7f)<<24 | uint32(sum[o+1])<<16 | uint32(sum[o+2])<<8 | uint32(sum[o+3])) % 1000000
	return fmt.Sprintf("%06d", v)
}

func TestDirectorySignInAsksForTheSecondStep(t *testing.T) {
	t.Setenv("JWT_SECRET", "ldap-two-step-integration-secret")
	t.Setenv("TOTP_KEK", "ldap-two-step-integration-kek")
	t.Setenv("LDAP_ENABLED", "true")
	t.Setenv("LDAP_HOST", "directory.example.test")
	t.Setenv("LDAP_BASE_DN", "dc=example,dc=test")
	t.Setenv("LDAP_USER_FILTER", "(uid=%s)")
	t.Setenv("LDAP_ADMIN_GROUPS", "")
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	integration.SetupRedis(t)
	oldLimit := helpers.SeatLimit
	t.Cleanup(func() { helpers.SeatLimit = oldLimit })
	helpers.SeatLimit = "" // directory sign-in is on licensed plans

	const directoryPassword = "the directory's password"
	t.Cleanup(authController.UseDirectoryForTest(func(_ *ldapService.LDAPClient, who, password string) (*ldapService.LDAPUser, error) {
		if password != directoryPassword {
			return nil, errors.New("invalid credentials")
		}
		return &ldapService.LDAPUser{DN: "uid=" + who + ",dc=example,dc=test", Email: who + "@example.test", Username: who}, nil
	}))

	// Two people the directory knows, as directory sign-in leaves them; one
	// has two-step on.
	member := func(name string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, signup_method, is_sso_managed, created_at, updated_at)
			VALUES ($1, $2, $3, 'ldap', true, NOW(), NOW())`, id, name+"@example.test", name); err != nil {
			t.Fatal(err)
		}
		return id
	}
	dana, eve := member("dana"), member("eve")
	enrolment, err := authBusiness.BeginTOTPEnrollment(ctx, dana, "dana@example.test")
	if err != nil {
		t.Fatalf("begin two-step: %v", err)
	}
	if _, err := authBusiness.ConfirmTOTPEnrollment(ctx, dana, totpCode(t, enrolment.Secret, time.Now())); err != nil {
		t.Fatalf("confirm two-step: %v", err)
	}

	post := func(h http.HandlerFunc, body any) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)))
		return rec
	}
	sessionCookie := func(rec *httptest.ResponseRecorder) bool {
		for _, c := range rec.Result().Cookies() {
			if c.Name == "Authorization" && c.Value != "" {
				return true
			}
		}
		return false
	}
	lastMethod := func(id uuid.UUID) string {
		t.Helper()
		var m *string
		if err := env.PG.QueryRow(`SELECT last_login_method FROM users WHERE id = $1`, id).Scan(&m); err != nil {
			t.Fatal(err)
		}
		if m == nil {
			return ""
		}
		return *m
	}
	signIn := func(who string) *httptest.ResponseRecorder {
		return post(authController.LDAPLogin, map[string]string{"username_or_email": who, "password": directoryPassword})
	}

	// 1. The directory's password alone is no session for someone with two-step on.
	rec := signIn("dana")
	var answer struct {
		Status    string `json:"status"`
		Challenge string `json:"challenge"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &answer)
	if rec.Code != http.StatusOK || answer.Status != "totp_required" || answer.Challenge == "" {
		t.Fatalf("directory sign-in with two-step on: %d %s, want a challenge", rec.Code, rec.Body.String())
	}
	if sessionCookie(rec) {
		t.Fatal("directory sign-in with two-step on set a session before the code")
	}
	if m := lastMethod(dana); m != "" {
		t.Errorf("a sign-in was recorded (%q) before the code", m)
	}

	// 2. The code completes it, recorded as a directory sign-in.
	rec = post(authController.TOTPLogin, map[string]string{"challenge": answer.Challenge, "code": totpCode(t, enrolment.Secret, time.Now().Add(30*time.Second))})
	if rec.Code != http.StatusOK || !sessionCookie(rec) {
		t.Fatalf("the code after a directory sign-in: %d %s, want a session", rec.Code, rec.Body.String())
	}
	if m := lastMethod(dana); m != "ldap" {
		t.Errorf("recorded as a %q sign-in, want ldap", m)
	}

	// 3. Without two-step on, the directory's password is enough, as before.
	rec = signIn("eve")
	if rec.Code != http.StatusOK || !sessionCookie(rec) {
		t.Fatalf("directory sign-in without two-step: %d %s, want a session", rec.Code, rec.Body.String())
	}
	if m := lastMethod(eve); m != "ldap" {
		t.Errorf("recorded as a %q sign-in, want ldap", m)
	}

	// 4. And a wrong directory password is refused.
	if rec := post(authController.LDAPLogin, map[string]string{"username_or_email": "dana", "password": "a guess"}); rec.Code != http.StatusUnauthorized || sessionCookie(rec) {
		t.Fatalf("a wrong directory password: %d", rec.Code)
	}
}
