package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// maxErrorBody is how much of an error answer a StatusError keeps: enough
// for the provider's message, and bounded whatever the provider sends.
const maxErrorBody = 2 << 10

// StatusRules is how one provider's answers are read. Every provider reads
// a status through Check, after handling what is its own (Miro's 404, a
// GraphQL error in a 200).
type StatusRules struct {
	// Name is the provider as people know it, in errors and the pause note.
	Name string
	// Wait is how long a 429 that names no wait holds the import.
	Wait time.Duration
	// Client is what SendJSON sends through: SharedAPIClient when nil, for a
	// provider whose hosts are its own. One whose host the admin types sets
	// SiteAPIClient.
	Client *http.Client
	// OnlyUnauthorized: only a 401 refuses the token; the provider answers
	// 403 about one thing the token can't reach.
	OnlyUnauthorized bool
}

// StatusError is a provider answering with an error status: its code, and
// the start of what it said.
type StatusError struct {
	Provider string
	Code     int
	Body     string
	// refused is a refusal of the token itself (see StatusRules).
	refused bool
}

func (e *StatusError) Error() string {
	if e.refused {
		return fmt.Sprintf("%s didn't accept the token (HTTP %d); reconnect it", e.Provider, e.Code)
	}
	if e.Body == "" {
		return fmt.Sprintf("%s HTTP %d", e.Provider, e.Code)
	}
	return fmt.Sprintf("%s HTTP %d: %s", e.Provider, e.Code, e.Body)
}

// Is makes a refused token match ErrTokenRejected.
func (e *StatusError) Is(target error) bool { return target == ErrTokenRejected && e.refused }

// Check is what an answer's status means to an import: nil below 400, a 429
// as *ErrRateLimited for as long as the provider asks (Wait when it names no
// time), and any other error status as *StatusError, which matches
// ErrTokenRejected when it refuses the token.
func (r StatusRules) Check(resp *http.Response) error {
	switch {
	case resp.StatusCode < 400:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return &ErrRateLimited{RetryAfter: helpers.RetryAfter(resp.Header, r.Wait), Reason: r.Name + " 429"}
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	return &StatusError{
		Provider: r.Name,
		Code:     resp.StatusCode,
		Body:     strings.TrimSpace(string(raw)),
		refused:  resp.StatusCode == http.StatusUnauthorized || (resp.StatusCode == http.StatusForbidden && !r.OnlyUnauthorized),
	}
}

// SendJSON sends req through r's client, reads the answer's status as Check
// does, and decodes a good answer into out: every provider's JSON read.
func (r StatusRules) SendJSON(req *http.Request, out any) error {
	client := r.Client
	if client == nil {
		client = SharedAPIClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := r.Check(resp); err != nil {
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
