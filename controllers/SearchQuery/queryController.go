// Package controllers (SearchQuery) answers /query: the one query grammar
// (design/QUERY_STACK_CONTRACT.md §4.8) read for the signed-in person, for
// the search field, a list's typed mode and the chips that write tokens.
package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	querySpec "github.com/akashc777/OneCamp/business/QuerySpec"
	business "github.com/akashc777/OneCamp/business/SearchQuery"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// parsesPerMinute is how many texts one person's fields may ask to be read a
// minute: a field asks after each pause in typing.
const parsesPerMinute = 600

// allowParse is the person's window, a seam for tests.
var allowParse = func(ctx context.Context, userUUID string) (bool, int) {
	res := redisStore.AllowFixedWindowOrLocal(ctx, registry.QueryParseRate, []string{userUUID}, parsesPerMinute)
	return res.Allowed, res.RetryAfterSeconds()
}

type parseRequest struct {
	Text  string          `json:"text"`
	Scope querySpec.Scope `json:"scope"`
	// Caret, a UTF-16 offset into Text, asks what to type there.
	Caret *int `json:"caret,omitempty"`
	// Mode is "search" (the default) or "list".
	Mode string `json:"mode,omitempty"`
}

type parseAnswer struct {
	Spec querySpec.Spec `json:"spec"`
	// Runs is whether the spec may run: false while an issue is an error.
	Runs        bool                  `json:"runs"`
	Issues      []business.Issue      `json:"issues"`
	Tokens      []business.Token      `json:"tokens"`
	Text        string                `json:"text"`
	Suggestions []business.Suggestion `json:"suggestions,omitempty"`
}

// ParseQuery POST /query/parse {text, scope, caret?, mode?} answers the text
// read as the signed-in person: the spec every compiler reads, whether it may
// run, the issues with their places and what to do, the tokens for colour
// and chips, the canonical text, and with a caret what to type there. A text
// that can't be read is still a 200: its issues say why.
func ParseQuery(w http.ResponseWriter, r *http.Request) {
	user, subject := accessBusiness.RequestSubject(r)
	if subject.UserUUID == "" {
		helpers.WriteUnauthorized(w)
		return
	}
	var req parseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Send the text to read as JSON: {\"text\": \"…\", \"scope\": {\"kind\": \"search\"}}."})
		return
	}
	if req.Scope.Kind == "" {
		req.Scope.Kind = querySpec.ScopeSearch
	}
	switch req.Scope.Kind {
	case querySpec.ScopeSearch, querySpec.ScopeMine:
	case querySpec.ScopeProject, querySpec.ScopeTable:
		if req.Scope.ID == "" {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say which " + req.Scope.Kind + " the query is for."})
			return
		}
	default:
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "There's no list called \"" + req.Scope.Kind + "\"."})
		return
	}
	mode := business.ModeSearch
	switch req.Mode {
	case "", "search":
	case "list":
		mode = business.ModeList
	default:
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "A query is read as a search or as a list."})
		return
	}
	if ok, retry := allowParse(r.Context(), subject.UserUUID); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"msg": "Too many searches in a minute. Wait a moment and try again.", "status": "rate_limited"})
		return
	}

	who := business.Asker{Subject: subject, Email: user.UserPostgresInfo.EmailID}
	spec, issues := business.Build(r.Context(), who, req.Text, req.Scope, mode)
	answer := parseAnswer{
		Spec:   spec,
		Runs:   business.Runs(issues),
		Issues: issues,
		Tokens: business.Parse(req.Text).Tokens,
		Text:   business.Format(r.Context(), who, spec),
	}
	if answer.Issues == nil {
		answer.Issues = []business.Issue{}
	}
	if answer.Tokens == nil {
		answer.Tokens = []business.Token{}
	}
	if req.Caret != nil {
		answer.Suggestions = business.Suggest(r.Context(), who, req.Text, *req.Caret, req.Scope)
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": answer})
}
