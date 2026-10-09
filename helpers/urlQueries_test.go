package helpers

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The error a Trello call gives when the network fails, built by net/http
// itself, keeps no key or token once it has been through WithoutURLQueries.
func TestAnErrorCarriesNoURLQuery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:1/1/members/me/boards?key=0123abcd&token=ATTA9876&fields=name", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, callErr := http.DefaultClient.Do(req)
	if callErr == nil {
		t.Fatal("nothing listens on port 1")
	}
	got := WithoutURLQueries(callErr.Error())
	if strings.Contains(got, "key=") || strings.Contains(got, "token=") || strings.Contains(got, "ATTA9876") {
		t.Errorf("the credentials are still there: %s", got)
	}
	if !strings.Contains(got, "http://127.0.0.1:1/1/members/me/boards") {
		t.Errorf("which call failed is lost: %s", got)
	}

	for in, want := range map[string]string{
		"no address here":                                     "no address here",
		"see https://trello.com/power-ups/admin.":             "see https://trello.com/power-ups/admin.",
		`{"url":"https://x.test/a?sig=1&b=2"}`:                `{"url":"https://x.test/a"}`,
		"two: https://a.test/x?t=1 and https://b.test/y#frag": "two: https://a.test/x and https://b.test/y",
	} {
		if got := WithoutURLQueries(in); got != want {
			t.Errorf("WithoutURLQueries(%q) = %q, want %q", in, got, want)
		}
	}
}
