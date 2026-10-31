package provider

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Every provider reads an answer's status one way: a 429 waits as long as
// the provider asks (or the provider's own wait), a refused token says so
// wherever it surfaces, and any other error status keeps its code and at most
// 2 KB of what the provider said.
func TestOneReadingOfAProvidersStatus(t *testing.T) {
	asana := StatusRules{Name: "Asana", Wait: time.Minute}
	linear := StatusRules{Name: "Linear", Wait: 30 * time.Second, OnlyUnauthorized: true}
	soon := time.Now().Add(45 * time.Second).UTC().Format(http.TimeFormat)
	long := strings.Repeat("x", 10<<10)
	for _, c := range []struct {
		what    string
		rules   StatusRules
		code    int
		retry   string
		body    string
		wait    time.Duration // a 429's wait, give or take a second
		refused bool
		plain   bool // a StatusError with the code
	}{
		{what: "a success", rules: asana, code: http.StatusOK},
		{what: "a 429 in seconds", rules: asana, code: http.StatusTooManyRequests, retry: "7", wait: 7 * time.Second},
		{what: "a 429 with a date", rules: asana, code: http.StatusTooManyRequests, retry: soon, wait: 45 * time.Second},
		{what: "a 429 naming nothing", rules: asana, code: http.StatusTooManyRequests, wait: time.Minute},
		{what: "a 401", rules: asana, code: http.StatusUnauthorized, refused: true, plain: true},
		{what: "a 403", rules: asana, code: http.StatusForbidden, refused: true, plain: true},
		{what: "a 403 where only 401 is about the token", rules: linear, code: http.StatusForbidden, plain: true},
		{what: "a 404", rules: asana, code: http.StatusNotFound, plain: true},
		{what: "a 500 with a long body", rules: asana, code: http.StatusInternalServerError, body: long, plain: true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c.retry != "" {
				w.Header().Set("Retry-After", c.retry)
			}
			w.WriteHeader(c.code)
			_, _ = w.Write([]byte(c.body))
		}))
		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		err = c.rules.Check(resp)
		_ = resp.Body.Close()
		srv.Close()

		if c.code < 400 {
			if err != nil {
				t.Errorf("%s: %v", c.what, err)
			}
			continue
		}
		wait, limited := IsRateLimited(err)
		if limited != (c.wait > 0) || (limited && (wait < c.wait-time.Second || wait > c.wait)) {
			t.Errorf("%s: rate limited %v for %v, want %v", c.what, limited, wait, c.wait)
		}
		if limited && !strings.Contains(err.Error(), c.rules.Name+" 429") {
			t.Errorf("%s: %q doesn't name the provider", c.what, err)
		}
		if errors.Is(err, ErrTokenRejected) != c.refused {
			t.Errorf("%s: refused token %v, want %v", c.what, errors.Is(err, ErrTokenRejected), c.refused)
		}
		var status *StatusError
		if errors.As(err, &status) != c.plain || (c.plain && (status.Code != c.code || status.Provider != c.rules.Name)) {
			t.Errorf("%s: %#v, want a StatusError %d from %s", c.what, err, c.code, c.rules.Name)
		}
		if len(err.Error()) > 2<<10+100 {
			t.Errorf("%s: the error keeps %d bytes of the body", c.what, len(err.Error()))
		}
	}
}
