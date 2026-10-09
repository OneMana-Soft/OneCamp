package business

// Saying what is wrong with a connection, in the admin's words.
//
// Connect used to save whatever was pasted. A token copied with a character
// missing, Trello's key and token swapped, a Jira site address with a typo or
// a scoped Jira token: each was saved as "connected", the workspace list then
// came back empty with no word why, and the first sign of trouble was a failed
// plan with "HTTP 401" in it. Now Connect asks the provider what the token can
// see before saving it, and anything that goes wrong talking to a provider is
// described by what the admin can do about it.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

// ProviderProblem is a provider failure as the admin is told it.
type ProviderProblem struct {
	// Status is the HTTP status to answer with. Never 401: the web app reads
	// a 401 as its own session ending.
	Status int
	// Code lets the web app offer the way out: "token_rejected" and
	// "not_connected" (reconnect), "unreachable", "rate_limited" or
	// "provider_error".
	Code string
	Msg  string
}

// connectTimeout bounds the test a connection gets before it is saved.
const connectTimeout = 25 * time.Second

// tokenHints is where each provider's credentials are, for the message when
// the provider refuses them.
var tokenHints = map[string]string{
	importModels.ProviderTrello:  "Trello didn't accept that API key and token. Copy both again from your Power-Up's API key page: the API key, then the token from the Token link beside it.",
	importModels.ProviderAsana:   "Asana didn't accept that token. Create a personal access token in Asana's developer console and paste it again.",
	importModels.ProviderJira:    "Jira didn't accept that email and API token. Use a classic API token (id.atlassian.com, Security, Create and manage API tokens, Create API token) with the email you sign in to Jira with.",
	importModels.ProviderNotion:  "Notion didn't accept that token. Copy the internal integration secret again from notion.so/my-integrations.",
	importModels.ProviderTodoist: "Todoist didn't accept that token. Copy your API token again from Todoist's Settings, Integrations, Developer.",
	importModels.ProviderLinear:  "Linear didn't accept that key. Create a personal API key in Linear's Settings, API, and paste it again.",
	importModels.ProviderClickUp: "ClickUp didn't accept that token. Copy your personal API token again from ClickUp's Settings, Apps.",
	"monday":                     "monday.com didn't accept that token. Open your avatar, Developers, My access tokens, and copy the personal API token again.",
}

// providerNames is how each provider is written in a sentence.
var providerNames = map[string]string{
	importModels.ProviderTrello:  "Trello",
	importModels.ProviderAsana:   "Asana",
	importModels.ProviderJira:    "Jira",
	importModels.ProviderNotion:  "Notion",
	importModels.ProviderTodoist: "Todoist",
	importModels.ProviderLinear:  "Linear",
	importModels.ProviderClickUp: "ClickUp",
	"monday":                     "monday.com",
	importModels.ProviderSlack:   "Slack",
}

func providerName(provider string) string {
	if n, ok := providerNames[provider]; ok {
		return n
	}
	return provider
}

// DescribeProviderError says what went wrong talking to a provider, and what
// to do. site is the address the admin typed for a self-addressed provider
// (Jira), or "". The provider's own message is passed on only when it is an
// answer from the provider: a network error carries the request's address,
// and for Trello that address holds the key and the token. Pure.
func DescribeProviderError(provider, site string, err error) ProviderProblem {
	name := providerName(provider)
	var urlErr *url.Error
	switch {
	case errors.Is(err, importModels.ErrTokenNotFound), errors.Is(err, importModels.ErrTokenUnreadable):
		return ProviderProblem{Status: http.StatusBadRequest, Code: "not_connected",
			Msg: name + " isn't connected any more. Connect it again, then try this again."}
	case errors.Is(err, importProvider.ErrTokenRejected):
		msg := tokenHints[provider]
		if msg == "" {
			msg = name + " didn't accept that token. Copy it again and reconnect."
		}
		return ProviderProblem{Status: http.StatusBadRequest, Code: "token_rejected", Msg: msg}
	case isRateLimited(err):
		return ProviderProblem{Status: http.StatusTooManyRequests, Code: "rate_limited",
			Msg: name + " is asking us to slow down. Try again in a minute."}
	case errors.As(err, &urlErr), errors.Is(err, context.DeadlineExceeded):
		if site != "" {
			return ProviderProblem{Status: http.StatusServiceUnavailable, Code: "unreachable",
				Msg: "Couldn't reach " + site + ". Check the site address (it looks like https://your-team.atlassian.net)."}
		}
		return ProviderProblem{Status: http.StatusServiceUnavailable, Code: "unreachable",
			Msg: "Couldn't reach " + name + ". Check this server can reach the internet, then try again."}
	}
	return ProviderProblem{Status: http.StatusServiceUnavailable, Code: "provider_error",
		Msg: name + " answered with an error: " + helpers.WithoutURLQueries(strings.TrimSpace(err.Error()))}
}

func isRateLimited(err error) bool {
	_, ok := importProvider.IsRateLimited(err)
	return ok
}

// TestConnection asks the provider what a token can see before it is saved,
// and returns that list (the workspace dropdown's first answer) or what is
// wrong. Providers without a list to ask for are saved untested.
func TestConnection(ctx context.Context, prov importProvider.Provider, ownerUserId string, tok *importModels.Token, site string) ([]importProvider.DiscoverItem, *ProviderProblem) {
	disc, ok := prov.(importProvider.Discoverer)
	if !ok {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	items, err := disc.Discover(ctx, ownerUserId, tok)
	if err != nil {
		p := DescribeProviderError(prov.Name(), site, err)
		return nil, &p
	}
	return items, nil
}

// DescribePlanError is a failed plan in the admin's words: a refused or
// missing token, a provider asking to slow down or one that can't be reached
// as DescribeProviderError says them, and anything else (a pick that's gone,
// a workspace still to choose) as the provider put it, as a sentence.
func DescribePlanError(ctx context.Context, job *importModels.Job, err error) ProviderProblem {
	var urlErr *url.Error
	if errors.Is(err, importModels.ErrTokenNotFound) || errors.Is(err, importModels.ErrTokenUnreadable) ||
		errors.Is(err, importProvider.ErrTokenRejected) ||
		isRateLimited(err) || errors.As(err, &urlErr) || errors.Is(err, context.DeadlineExceeded) {
		return DescribeProviderError(job.Provider, connectedSite(ctx, job), err)
	}
	return ProviderProblem{Status: http.StatusBadRequest, Code: "plan_failed", Msg: sentence(helpers.WithoutURLQueries(err.Error()))}
}

// sentence is an error's text as a sentence: a capital first letter and a
// full stop. Pure.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	s = string(r)
	if !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, "!") && !strings.HasSuffix(s, "?") {
		s += "."
	}
	return s
}
