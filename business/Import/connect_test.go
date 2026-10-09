package business

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

func TestAProviderFailureIsSaidByWhatTheAdminCanDo(t *testing.T) {
	// A refused token, in each provider's words, says where to get a good one.
	refused := &importProvider.TokenRejected{Msg: "trello auth failed (HTTP 401); reconnect token"}
	p := DescribeProviderError(importModels.ProviderTrello, "", fmt.Errorf("discover: %w", refused))
	if p.Code != "token_rejected" || p.Status != http.StatusBadRequest || !strings.Contains(p.Msg, "Power-Up") {
		t.Errorf("trello refused: %+v", p)
	}
	if p := DescribeProviderError(importModels.ProviderJira, "https://acme.atlassian.net", refused); !strings.Contains(p.Msg, "classic API token") {
		t.Errorf("jira refused: %+v", p)
	}
	if p := DescribeProviderError("wrike", "", refused); p.Msg != "wrike didn't accept that token. Copy it again and reconnect." {
		t.Errorf("a provider without a hint: %+v", p)
	}
	// Never a 401: the web app reads that as its own session ending.
	for _, err := range []error{refused, importModels.ErrTokenNotFound, &importProvider.ErrRateLimited{}, errors.New("boom")} {
		if p := DescribeProviderError(importModels.ProviderAsana, "", err); p.Status == http.StatusUnauthorized {
			t.Errorf("%v answered 401", err)
		}
	}

	if p := DescribeProviderError(importModels.ProviderAsana, "", importModels.ErrTokenNotFound); p.Code != "not_connected" || !strings.Contains(p.Msg, "Asana isn't connected any more") {
		t.Errorf("no token: %+v", p)
	}
	// A saved token the server can't decrypt any more is as good as none.
	unreadable := fmt.Errorf("decrypt access: %w: %w", importModels.ErrTokenUnreadable, errors.New("cipher: message authentication failed"))
	if p := DescribeProviderError(importModels.ProviderAsana, "", unreadable); p.Code != "not_connected" {
		t.Errorf("an unreadable token: %+v", p)
	}
	if p := DescribeProviderError(importModels.ProviderLinear, "", &importProvider.ErrRateLimited{Reason: "linear 429"}); p.Code != "rate_limited" || p.Status != http.StatusTooManyRequests {
		t.Errorf("slow down: %+v", p)
	}

	// Unreachable: the site the admin typed is named, and the request's own
	// address (which for Trello carries the key and the token) is not.
	netErr := &url.Error{Op: "Get", URL: "https://api.trello.com/1/members/me/boards?key=SECRETKEY&token=SECRETTOKEN", Err: errors.New("dial tcp: lookup api.trello.com: no such host")}
	p = DescribeProviderError(importModels.ProviderTrello, "", netErr)
	if p.Code != "unreachable" || strings.Contains(p.Msg, "SECRET") || !strings.Contains(p.Msg, "Couldn't reach Trello") {
		t.Errorf("trello unreachable: %+v", p)
	}
	p = DescribeProviderError(importModels.ProviderJira, "https://acme.atlasian.net", &url.Error{Op: "Get", URL: "https://acme.atlasian.net/rest/api/3/project/search", Err: errors.New("no such host")})
	if p.Msg != "Couldn't reach https://acme.atlasian.net. Check the site address (it looks like https://your-team.atlassian.net)." {
		t.Errorf("jira site unreachable: %+v", p)
	}
	if p := DescribeProviderError(importModels.ProviderNotion, "", context.DeadlineExceeded); p.Code != "unreachable" {
		t.Errorf("a timeout: %+v", p)
	}

	// Anything else is the provider's own answer.
	if p := DescribeProviderError(importModels.ProviderNotion, "", errors.New("notion HTTP 400: bad request")); p.Code != "provider_error" || p.Msg != "Notion answered with an error: notion HTTP 400: bad request" {
		t.Errorf("an error answer: %+v", p)
	}
}

func TestAPlanThatFailsSaysWhyAsASentence(t *testing.T) {
	job := &importModels.Job{Provider: importModels.ProviderAsana}
	p := DescribePlanError(context.Background(), job, errors.New("pick which Asana workspace to import: this token can see 2 (A, B)"))
	if p.Code != "plan_failed" || p.Msg != "Pick which Asana workspace to import: this token can see 2 (A, B)." {
		t.Errorf("%+v", p)
	}
	if p := DescribePlanError(context.Background(), job, &importProvider.TokenRejected{Msg: "asana auth failed (HTTP 401)"}); p.Code != "token_rejected" {
		t.Errorf("a refused token at plan time: %+v", p)
	}
	// A saved token that can't be read (the key it was sealed with changed)
	// is connected again, as at the workspace list: the dialog offers
	// Reconnect.
	if p := DescribePlanError(context.Background(), job, fmt.Errorf("load token: %w", importModels.ErrTokenUnreadable)); p.Code != "not_connected" {
		t.Errorf("an unreadable token at plan time: %+v", p)
	}
	// Said without a URL's query string, as every other provider error.
	if p := DescribePlanError(context.Background(), job, errors.New("trello GET https://api.trello.com/1/boards/b1/lists?key=0123abcd&token=ATTA9876: unexpected answer")); p.Code != "plan_failed" ||
		strings.Contains(p.Msg, "ATTA9876") || strings.Contains(p.Msg, "key=") || !strings.Contains(p.Msg, "https://api.trello.com/1/boards/b1/lists") {
		t.Errorf("a plan error naming a URL: %+v", p)
	}
	for in, want := range map[string]string{"": "", "done.": "Done.", "why?": "Why?", "élan": "Élan."} {
		if got := sentence(in); got != want {
			t.Errorf("sentence(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOnlyAWaitingImportIsDiscarded(t *testing.T) {
	for status, waiting := range map[string]bool{
		importModels.StatusPending: true, importModels.StatusValidating: true, importModels.StatusPlanned: true,
		importModels.StatusRunning: false, importModels.StatusPaused: false, importModels.StatusCompleted: false,
		importModels.StatusFailed: false, importModels.StatusCancelled: false, importModels.StatusRolledBack: false,
	} {
		if IsWaiting(status) != waiting {
			t.Errorf("IsWaiting(%s) = %v", status, !waiting)
		}
	}
}

// discoverer is a provider whose list answers what the test says.
type discoverer struct {
	importProvider.Provider
	items []importProvider.DiscoverItem
	err   error
}

func (d discoverer) Name() string { return importModels.ProviderTodoist }
func (d discoverer) Discover(context.Context, string, *importModels.Token) ([]importProvider.DiscoverItem, error) {
	return d.items, d.err
}

func TestConnectingAsksTheProviderFirst(t *testing.T) {
	tok := &importModels.Token{AccessToken: "tok"}
	items, problem := TestConnection(context.Background(), discoverer{items: []importProvider.DiscoverItem{{ID: "p1", Name: "Launch"}}}, "u", tok, "")
	if problem != nil || len(items) != 1 {
		t.Fatalf("a good token: %v %+v", items, problem)
	}
	_, problem = TestConnection(context.Background(), discoverer{err: &importProvider.TokenRejected{Msg: "todoist auth failed (HTTP 401)"}}, "u", tok, "")
	if problem == nil || problem.Code != "token_rejected" || !strings.Contains(problem.Msg, "Todoist didn't accept that token") {
		t.Fatalf("a refused token: %+v", problem)
	}
}

// A provider error that isn't a network error is passed on in words, and a
// URL in it keeps no query string: Trello's carries the key and token.
func TestAProviderErrorNeverCarriesAURLsQuery(t *testing.T) {
	err := fmt.Errorf("trello GET https://api.trello.com/1/boards/b1/cards?key=0123abcd&token=ATTA9876&limit=1000: HTTP 500")
	p := DescribeProviderError(importModels.ProviderTrello, "", err)
	if strings.Contains(p.Msg, "key=") || strings.Contains(p.Msg, "ATTA9876") || !strings.Contains(p.Msg, "https://api.trello.com/1/boards/b1/cards") {
		t.Errorf("said: %s", p.Msg)
	}
}
