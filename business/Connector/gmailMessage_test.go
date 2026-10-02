package business

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"google.golang.org/api/googleapi"
)

func decodeRaw(t *testing.T, raw string) string {
	t.Helper()
	b, err := base64.URLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("not base64url: %v", err)
	}
	return string(b)
}

func TestRawEmailRefusesInjectedHeadersInAnyField(t *testing.T) {
	for _, h := range []mailHeader{
		{"To", "a@x.com\r\nBcc: spy@evil"},
		{"Subject", "Hi\nX-Evil: 1"},
		{"References", "<a@x>\r\nBcc: spy@evil"},
	} {
		if _, err := rawEmail([]mailHeader{{"To", "ok@x.com"}, h}, "body"); !errors.Is(err, ErrBadHeader) {
			t.Errorf("%s with a line break: err=%v", h.name, err)
		}
	}
}

func TestRawEmailEncodesNonASCIIAndNormalisesLineEndings(t *testing.T) {
	raw, err := rawEmail([]mailHeader{{"To", "José Núñez <jose@x.com>"}, {"Subject", "Re: Café menu"}, {"In-Reply-To", ""}}, "one\ntwo")
	if err != nil {
		t.Fatal(err)
	}
	msg := decodeRaw(t, raw)
	head, body, ok := strings.Cut(msg, "\r\n\r\n")
	if !ok {
		t.Fatalf("no header/body split:\n%s", msg)
	}
	if strings.Contains(head, "Café") || strings.Contains(head, "José") {
		t.Errorf("non-ASCII header sent raw:\n%s", head)
	}
	if !strings.Contains(head, "=?utf-8?") || !strings.Contains(head, "<jose@x.com>") {
		t.Errorf("header not encoded as expected:\n%s", head)
	}
	if strings.Contains(head, "In-Reply-To") {
		t.Error("an empty header was written")
	}
	if body != "one\r\ntwo" {
		t.Errorf("body line endings: %q", body)
	}
}

func TestCutBytesNeverSplitsACharacter(t *testing.T) {
	s := strings.Repeat("é", 10) // 2 bytes each
	got, cut := cutBytes(s, 5)
	if !cut || !utf8.ValidString(got) || got != "éé" {
		t.Fatalf("got %q cut=%v", got, cut)
	}
	if got, cut := cutBytes("short", 50); cut || got != "short" {
		t.Fatalf("short string changed: %q %v", got, cut)
	}
}

func TestClassifyAPIError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want APIProblem
	}{
		{"nil", nil, ProblemNone},
		{"revoked", &googleapi.Error{Code: 401}, ProblemExpired},
		{"refresh refused", fmt.Errorf("oauth2: invalid_grant"), ProblemExpired},
		{"scope", &googleapi.Error{Code: 403, Message: "Request had insufficient authentication scopes."}, ProblemPermissions},
		{"api off", &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "accessNotConfigured"}}}, ProblemAPIDisabled},
		{"api off text", &googleapi.Error{Code: 403, Message: "Gmail API has not been used in project 1"}, ProblemAPIDisabled},
		{"quota as 403", &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "userRateLimitExceeded"}}}, ProblemRateLimited},
		{"429", &googleapi.Error{Code: 429}, ProblemRateLimited},
		{"gone", fmt.Errorf("gmail thread: %w", &googleapi.Error{Code: 404}), ProblemNotFound},
		{"outage", &googleapi.Error{Code: 503}, ProblemUnknown},
	}
	for _, c := range cases {
		if got := ClassifyAPIError(c.err); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}

func TestGmailThreadURLNamesTheAccount(t *testing.T) {
	if got := GmailThreadURL("sam+work@acme.com", "18c2"); got != "https://mail.google.com/mail/?authuser=sam%2Bwork%40acme.com#all/18c2" {
		t.Errorf("got %s", got)
	}
	if got := GmailThreadURL("", "18c2"); got != "https://mail.google.com/mail/u/0/#all/18c2" {
		t.Errorf("fallback: %s", got)
	}
}

func TestStateBindingCarriesOnlyKnownReturnPages(t *testing.T) {
	u := "6f1c1f43-6a42-4b7e-9c0f-6d0f5f3a8a11"
	cases := map[string]string{
		u + ":gmail":                        "",      // issued before return pages existed
		u + ":gmail:inbox":                  "inbox", // known page
		u + ":gmail:https://evil.example/x": "",      // never a URL
		u + ":gmail:":                       "",
	}
	for bound, want := range cases {
		res, err := parseStateBinding(bound)
		if err != nil {
			t.Fatalf("%q: %v", bound, err)
		}
		if res.ProviderID != "gmail" || res.UserUUID.String() != u || res.ReturnPage != want {
			t.Errorf("%q: got %+v, want return %q", bound, res, want)
		}
	}
	if _, err := parseStateBinding("nouser"); err == nil {
		t.Error("a malformed binding was accepted")
	}
}
