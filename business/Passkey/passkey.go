// Package business (Passkey) is sign-in with passkeys (WebAuthn): adding one
// while signed in, and signing in with one from the sign-in page.
//
// THE RULES THAT MATTER.
//   - A passkey proves possession AND the person (user verification is
//     required), so it is a full sign-in and needs no second factor, the way
//     Google and GitHub treat them.
//   - It never outranks the workspace's identity provider: an account the IdP
//     manages signs in through the IdP, so offboarding there still works.
//   - Each ceremony's challenge is kept in Redis for five minutes and read back
//     exactly once, so an answer can't be replayed.
//   - The relying party is the web app's own host (FE_HOST_DOMAIN). Passkeys
//     are bound to it: moving the workspace to a new domain means adding them
//     again, which the settings page says.
package business

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/akashc777/OneCamp/helpers"
	passkeyModel "github.com/akashc777/OneCamp/models/postgres/Passkey"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
)

// Error is a refusal written for the person; it carries no detail an attacker
// could use.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

var (
	ErrNotConfigured = &Error{"Passkeys aren't set up on this server yet. Ask your admin to set the web app's address (FE_HOST_DOMAIN)."}
	errExpired       = &Error{"That took too long. Try again."}
	errSignIn        = &Error{"That passkey didn't work. Try again, or sign in another way."}
)

// RelyingParty is who passkeys are made for: the web app's host. Pure apart
// from the environment it is given.
func RelyingParty(feHost string) (id string, origins []string, err error) {
	host := strings.TrimSpace(feHost)
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host = strings.TrimSuffix(host, "/")
	if host == "" || strings.ContainsAny(host, "/ ") {
		return "", nil, ErrNotConfigured
	}
	id = host
	if h, _, splitErr := net.SplitHostPort(host); splitErr == nil {
		id = h
	}
	origins = []string{"https://" + host}
	// Development: the web app runs over plain http on localhost.
	if id == "localhost" || id == "127.0.0.1" {
		origins = append(origins, "http://"+host, "http://"+id+":3000")
	}
	return id, origins, nil
}

var (
	rpMu     sync.Mutex
	rpCached *webauthn.WebAuthn
	rpFor    string
)

func relyingParty() (*webauthn.WebAuthn, error) {
	host := os.Getenv("FE_HOST_DOMAIN")
	rpMu.Lock()
	defer rpMu.Unlock()
	if rpCached != nil && rpFor == host {
		return rpCached, nil
	}
	id, origins, err := RelyingParty(host)
	if err != nil {
		return nil, err
	}
	w, err := webauthn.New(&webauthn.Config{
		RPID:          id,
		RPDisplayName: "OneCamp",
		RPOrigins:     origins,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationRequired,
		},
	})
	if err != nil {
		return nil, err
	}
	rpCached, rpFor = w, host
	return w, nil
}

// person is someone to WebAuthn: their id is their user id's 16 bytes, which
// is what a passkey hands back as its user handle.
type person struct {
	owner *passkeyModel.Owner
	creds []webauthn.Credential
}

func (p *person) WebAuthnID() []byte                         { b, _ := p.owner.UserID.MarshalBinary(); return b }
func (p *person) WebAuthnName() string                       { return p.owner.Email }
func (p *person) WebAuthnDisplayName() string                { return p.owner.Name }
func (p *person) WebAuthnCredentials() []webauthn.Credential { return p.creds }

func load(owner *passkeyModel.Owner) (*person, error) {
	keys, err := passkeyModel.List(owner.UserID)
	if err != nil {
		return nil, err
	}
	p := &person{owner: owner}
	for _, k := range keys {
		var c webauthn.Credential
		if json.Unmarshal(k.Credential, &c) == nil {
			p.creds = append(p.creds, c)
		}
	}
	return p, nil
}

// ceremony is what the server remembers between asking and the answer.
type ceremony struct {
	Session webauthn.SessionData `json:"session"`
	UserID  uuid.UUID            `json:"user_id,omitempty"`
}

func remember(ctx context.Context, c ceremony) (string, error) {
	id := uuid.NewString()
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return id, redisStore.SetString(ctx, registry.PasskeyCeremony, []string{id}, string(b))
}

func recall(ctx context.Context, id string) (*ceremony, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, errExpired
	}
	raw, found, err := redisStore.GetDelString(ctx, registry.PasskeyCeremony, []string{id})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errExpired
	}
	var c ceremony
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return nil, errExpired
	}
	return &c, nil
}

// CanSignIn says whether an account may sign in with a passkey at all.
func CanSignIn(o *passkeyModel.Owner) error {
	switch {
	case o == nil, o.IsExternal:
		return errSignIn
	case o.IsSSOManaged:
		return &Error{"This account signs in through your company's sign-in. Use that instead."}
	}
	return nil
}

// BeginRegistration starts adding a passkey for a signed-in person.
func BeginRegistration(ctx context.Context, userID uuid.UUID) (*protocol.CredentialCreation, string, error) {
	owner, err := passkeyModel.OwnerOf(userID)
	if err != nil {
		return nil, "", err
	}
	if err := CanSignIn(owner); err != nil {
		return nil, "", err
	}
	rp, err := relyingParty()
	if err != nil {
		return nil, "", err
	}
	p, err := load(owner)
	if err != nil {
		return nil, "", err
	}
	if len(p.creds) >= passkeyModel.MaxPerPerson {
		return nil, "", &Error{fmt.Sprintf("You have %d passkeys, the most one person can keep. Remove one first.", passkeyModel.MaxPerPerson)}
	}
	exclude := make([]protocol.CredentialDescriptor, 0, len(p.creds))
	for _, c := range p.creds {
		exclude = append(exclude, c.Descriptor())
	}
	options, session, err := rp.BeginRegistration(p,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithExclusions(exclude),
	)
	if err != nil {
		return nil, "", err
	}
	id, err := remember(ctx, ceremony{Session: *session, UserID: userID})
	return options, id, err
}

// PasskeyName tidies the name a person gives a passkey. Pure.
func PasskeyName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		name = "Passkey"
	}
	if utf8.RuneCountInString(name) > 60 {
		return "", &Error{"Keep the name under 60 characters."}
	}
	return name, nil
}

// FinishRegistration stores the passkey the browser made.
func FinishRegistration(ctx context.Context, userID uuid.UUID, ceremonyID, name string, body io.Reader) (*passkeyModel.Passkey, error) {
	name, err := PasskeyName(name)
	if err != nil {
		return nil, err
	}
	c, err := recall(ctx, ceremonyID)
	if err != nil {
		return nil, err
	}
	if c.UserID != userID {
		return nil, errExpired
	}
	rp, err := relyingParty()
	if err != nil {
		return nil, err
	}
	owner, err := passkeyModel.OwnerOf(userID)
	if err != nil {
		return nil, err
	}
	if err := CanSignIn(owner); err != nil {
		return nil, err
	}
	p, err := load(owner)
	if err != nil {
		return nil, err
	}
	parsed, err := protocol.ParseCredentialCreationResponseBody(body)
	if err != nil {
		return nil, &Error{"Your browser's answer couldn't be read. Try again."}
	}
	cred, err := rp.CreateCredential(p, c.Session, parsed)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Passkey/FinishRegistration verify err: %v", err)
		return nil, &Error{"That passkey couldn't be added. Try again."}
	}
	stored, err := json.Marshal(cred)
	if err != nil {
		return nil, err
	}
	if existing, err := passkeyModel.ByCredentialID(cred.ID); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, &Error{"That passkey is already added."}
	}
	return passkeyModel.Create(userID, cred.ID, stored, name)
}

// BeginLogin starts a sign-in with any passkey for this server.
func BeginLogin(ctx context.Context) (*protocol.CredentialAssertion, string, error) {
	rp, err := relyingParty()
	if err != nil {
		return nil, "", err
	}
	options, session, err := rp.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, "", err
	}
	id, err := remember(ctx, ceremony{Session: *session})
	return options, id, err
}

// FinishLogin checks the browser's answer and says who signed in.
func FinishLogin(ctx context.Context, ceremonyID string, body io.Reader, now time.Time) (uuid.UUID, error) {
	c, err := recall(ctx, ceremonyID)
	if err != nil {
		return uuid.Nil, err
	}
	rp, err := relyingParty()
	if err != nil {
		return uuid.Nil, err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(body)
	if err != nil {
		return uuid.Nil, errSignIn
	}
	var signedIn *person
	var refusal error
	user, cred, err := rp.ValidatePasskeyLogin(func(rawID, userHandle []byte) (webauthn.User, error) {
		key, err := passkeyModel.ByCredentialID(rawID)
		if err != nil || key == nil {
			return nil, errors.New("unknown passkey")
		}
		handle, _ := key.UserID.MarshalBinary()
		if !bytes.Equal(handle, userHandle) {
			return nil, errors.New("passkey and user handle disagree")
		}
		owner, err := passkeyModel.OwnerOf(key.UserID)
		if err != nil {
			return nil, err
		}
		if refusal = CanSignIn(owner); refusal != nil {
			return nil, refusal
		}
		signedIn, err = load(owner)
		return signedIn, err
	}, c.Session, parsed)
	if refusal != nil {
		return uuid.Nil, refusal
	}
	if err != nil || user == nil || cred == nil || signedIn == nil {
		helpers.LogErrorWithContext(ctx, "business/Passkey/FinishLogin refused: %v", err)
		return uuid.Nil, errSignIn
	}
	if cred.Authenticator.CloneWarning {
		helpers.LogErrorWithContext(ctx, "business/Passkey/FinishLogin: possible cloned authenticator for user %s", signedIn.owner.UserID)
		return uuid.Nil, errSignIn
	}
	if stored, err := json.Marshal(cred); err == nil {
		if err := passkeyModel.Used(cred.ID, stored, now); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Passkey/FinishLogin record use err: %v", err)
		}
	}
	return signedIn.owner.UserID, nil
}
