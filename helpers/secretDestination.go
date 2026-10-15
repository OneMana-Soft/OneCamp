package helpers

// A secret stored next to an address is bound to that address.
//
// WHY. Several things this server connects to keep a credential beside the
// address it is sent to: an MCP server's token, a remote agent's secret. Their
// forms never show the stored secret, so an edit that leaves the secret field
// blank keeps it, which is right for a rename. It was also what happened when
// only the address changed: one save sent the stored token to whatever address
// was typed, an admin's GitHub token to anybody's server.
//
// THE RULE. A stored secret goes only to the destination it was given for: the
// same scheme, host and port, attached the same way (an auth type, a header
// name). Moving any of those without entering the secret again is refused, in
// words the person can act on, and the stored secret is never sent anywhere
// new. A different path on the same origin keeps it: that is the same service,
// and a server that wants it elsewhere on itself already has it.
//
// One function for every surface that stores a secret beside an address, so
// they cannot disagree about what "the same address" means. The web mirrors it
// (lib/utils/secretDestination.ts), and testdata/secret_destination.json holds
// the cases both run.

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// SecretDestination is where a stored secret is sent and how it is attached.
type SecretDestination struct {
	// URL is the address the secret goes to. Only its origin counts.
	URL string
	// How is how the secret is attached, in the surface's own terms: an auth
	// type, a header name. Compared without regard to case or surrounding
	// space, because header names are case-insensitive.
	How []string
}

// SecretMoves reports whether a secret kept for from would reach somewhere new
// at to: another scheme, host or port, or attached another way. An address that
// doesn't parse counts as moved, so a doubtful case asks for the secret again
// rather than sending it; giving an address where there was none, or taking it
// away, moves it too.
func SecretMoves(from, to SecretDestination) bool {
	// No address on either side is the surface's built-in destination (a
	// search provider's own API), so only how it is sent can move it.
	if strings.TrimSpace(from.URL) == "" && strings.TrimSpace(to.URL) == "" {
		return !sameHow(from.How, to.How)
	}
	a, okA := secretOrigin(from.URL)
	b, okB := secretOrigin(to.URL)
	if !okA || !okB || a != b {
		return true
	}
	return !sameHow(from.How, to.How)
}

// SecretReentryError is a save that would carry a stored secret to a new
// destination. Its message is what the person reads.
type SecretReentryError struct {
	// Noun names the secret as the form does: "token", "secret", "API key".
	Noun string
}

func (e *SecretReentryError) Error() string {
	noun := strings.TrimSpace(e.Noun)
	if noun == "" {
		noun = "token"
	}
	return "Enter the " + noun + " again for the new address."
}

// IsSecretReentry reports whether err is that refusal, so a controller can
// answer it with a 400 and the web can put it under the secret field.
func IsSecretReentry(err error) bool {
	var re *SecretReentryError
	return errors.As(err, &re)
}

// CheckKeptSecret is the check every such surface makes on save. stored says a
// secret is kept for from; given says a new one came with this save. It is nil
// when the save may go ahead: a new secret was given, none is stored, or the
// destination hasn't moved. Otherwise it is a *SecretReentryError naming the
// secret as noun.
func CheckKeptSecret(stored, given bool, from, to SecretDestination, noun string) error {
	if given || !stored {
		return nil
	}
	if SecretMoves(from, to) {
		return &SecretReentryError{Noun: noun}
	}
	return nil
}

// origin is an address's scheme, host and port, normalised so two spellings of
// one origin compare equal.
type origin struct{ scheme, host, port string }

// secretOrigin reads raw's origin: the scheme and host lower-cased, a trailing
// dot dropped from the host, and the scheme's default port filled in. ok is
// false for anything without a host.
func secretOrigin(raw string) (origin, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return origin{}, false
	}
	o := origin{
		scheme: strings.ToLower(u.Scheme),
		host:   strings.TrimSuffix(strings.ToLower(u.Hostname()), "."),
		port:   u.Port(),
	}
	if o.host == "" {
		return origin{}, false
	}
	if o.port == "" {
		switch o.scheme {
		case "http", "ws":
			o.port = "80"
		case "https", "wss":
			o.port = "443"
		}
	}
	return o, true
}

// sameHow compares two attachment descriptions, entry by entry, ignoring case
// and surrounding space.
func sameHow(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(strings.TrimSpace(a[i]), strings.TrimSpace(b[i])) {
			return false
		}
	}
	return true
}

// WriteSecretAwareBadRequest answers err as a 400, marking a
// SecretReentryError with code "secret_reentry" so the web can put the
// sentence under the secret field and make that field required.
func WriteSecretAwareBadRequest(w http.ResponseWriter, err error) {
	body := Envolope{"msg": err.Error()}
	if IsSecretReentry(err) {
		body["code"] = "secret_reentry"
	}
	WriteJSON(w, http.StatusBadRequest, body)
}
