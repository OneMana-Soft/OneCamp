package controllers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/google/uuid"
)

// A list of docs answers only the scopes and cursors it names, before it reads
// anything: an unknown scope listed the workspace, and a made-up cursor could
// start anywhere.
func TestAListOfDocsRefusesAScopeOrCursorItNeverGave(t *testing.T) {
	for _, query := range []string{"scope=everything", "cursor=" + base64.RawURLEncoding.EncodeToString([]byte("-5")), "cursor=not*base64"} {
		rec := httptest.NewRecorder()
		ListDocsV1(rec, httptest.NewRequest(http.MethodGet, "/v1/docs?"+query, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("?%s answered %d, want 400: %s", query, rec.Code, rec.Body)
		}
	}
}

// Each error from the docs answers as the API documents it. A failure that
// is nobody's request's fault is a failed read (503, try again) on a read, as
// every read answers one, and a 500 on a write.
func TestADocErrorAnswersWithItsStatus(t *testing.T) {
	for _, read := range []bool{true, false} {
		down := http.StatusInternalServerError
		if read {
			down = http.StatusServiceUnavailable
		}
		for err, want := range map[error]int{
			contentIOBusiness.ErrNotFound:                        http.StatusNotFound,
			collab.ErrDocInTrash:                                 http.StatusNotFound,
			contentIOBusiness.ErrForbidden:                       http.StatusForbidden,
			contentIOBusiness.ErrEmpty:                           http.StatusBadRequest,
			contentcodec.ErrTooDeep:                              http.StatusBadRequest,
			contentcodec.ErrTooCostly:                            http.StatusBadRequest,
			contentIOBusiness.ErrUnavailable:                     http.StatusServiceUnavailable,
			fmt.Errorf("append: %w", collab.ErrUnavailable):      http.StatusServiceUnavailable,
			&collab.OpError{Op: "append", Status: 409, Msg: "x"}: http.StatusUnprocessableEntity,
			errors.New("dgraph is down"):                         down,
		} {
			rec := httptest.NewRecorder()
			docError(rec, httptest.NewRequest(http.MethodGet, "/v1/docs/x", nil), err, read)
			if rec.Code != want {
				t.Errorf("%v (read %v) answered %d, want %d", err, read, rec.Code, want)
			}
			if strings.Contains(rec.Body.String(), "dgraph") {
				t.Errorf("%v: the answer names the store: %s", err, rec.Body)
			}
		}
	}
}

// Markdown that nests quotes or lists past what the reader takes is refused,
// saying why, before anything reads it: a megabyte of ">" (the API's whole
// body) held a request for well over ten minutes of parsing, and half a
// megabyte took six minutes and half a gigabyte of stack.
func TestMarkdownNestedTooDeepIsRefusedSayingWhy(t *testing.T) {
	deep := strings.Repeat(">", 1<<20-64) + " x"
	rec := httptest.NewRecorder()
	CreateDocV1(rec, httptest.NewRequest(http.MethodPost, "/v1/docs", strings.NewReader(`{"title":"Deep","markdown":"`+deep+`"}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "levels deep") {
		t.Fatalf("a megabyte of nested quotes answered %d: %.200s", rec.Code, rec.Body)
	}
}

// A list that couldn't be read is a failed read, 503 with try again, as every
// read answers one: it said 500, and nothing logged why.
func TestAListOfDocsThatCouldntBeReadSaysTryAgain(t *testing.T) {
	was := listDocs
	t.Cleanup(func() { listDocs = was })
	listDocs = func(context.Context, accessBusiness.Subject, string, int, int) ([]contentIOBusiness.ListedDoc, bool, error) {
		return nil, false, errors.New("dgraph is down")
	}
	rec := httptest.NewRecorder()
	ListDocsV1(rec, httptest.NewRequest(http.MethodGet, "/v1/docs", nil))
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "dgraph") {
		t.Fatalf("a list that failed answered %d: %s", rec.Code, rec.Body)
	}
}

// A token bound to an agent acts as its owner with the agent beside them
// (I11), so what it writes is credited to both; and a token never carries its
// owner's admin powers (least privilege, the owner's decision of 16 Oct 2026),
// so an admin's token is refused what only an admin may do.
func TestAnAPICallActsAsTheTokensOwnerWithItsAgent(t *testing.T) {
	owner := userModels.UserInfo{}
	owner.UserPostgresInfo.Id = uuid.New()
	owner.UserPostgresInfo.IsAdmin = true
	// As the token's middleware reads them: the account and its graph node,
	// one id (middleware/apiToken.go).
	owner.UserDgraphInfo.Uuid = owner.UserPostgresInfo.Id.String()
	owner.UserDgraphInfo.Uid = "0x2a"
	ctx := context.WithValue(context.Background(), helpers.UserInfoContextKey, owner)
	plain := httptest.NewRequest(http.MethodPost, "/v1/docs", nil).WithContext(ctx)
	if got := apiSubject(plain); got.UserUID != "0x2a" || got.UserUUID != owner.UserPostgresInfo.Id.String() || got.AgentUUID != "" {
		t.Fatalf("a plain token acts as %+v", got)
	}
	if got := apiSubject(plain); got.Admin || got.ActingAdmin() {
		t.Fatalf("an admin's token acts as an admin: %+v", got)
	}
	agent := uuid.NewString()
	bound := plain.WithContext(context.WithValue(ctx, helpers.ApiAgentIDContextKey, agent))
	if got := apiSubject(bound); got.AgentUUID != agent || got.UserUUID != owner.UserPostgresInfo.Id.String() {
		t.Fatalf("an agent's token acts as %+v, want its owner with agent %s", got, agent)
	}
}

// A cursor the list gave starts the next page where it said, with the limit
// asked for (held to the most a page holds), and the page hands out the
// cursor for the one after it.
func TestAListOfDocsPagesFromTheCursorItGave(t *testing.T) {
	was := listDocs
	t.Cleanup(func() { listDocs = was })
	var gotLimit, gotOffset int
	listDocs = func(_ context.Context, _ accessBusiness.Subject, _ string, limit, offset int) ([]contentIOBusiness.ListedDoc, bool, error) {
		gotLimit, gotOffset = limit, offset
		return nil, true, nil
	}
	for _, c := range []struct {
		query                string
		limit, offset, after int
	}{
		{"limit=5&cursor=" + helpers.OffsetCursor(10), 5, 10, 15},
		{"", apiDocPage, 0, apiDocPage},
		{fmt.Sprintf("limit=%d", apiDocPageMax+1), apiDocPageMax, 0, apiDocPageMax},
	} {
		rec := httptest.NewRecorder()
		ListDocsV1(rec, httptest.NewRequest(http.MethodGet, "/v1/docs?"+c.query, nil))
		if rec.Code != http.StatusOK || gotLimit != c.limit || gotOffset != c.offset {
			t.Errorf("%q: answered %d listing %d from %d, want 200 listing %d from %d", c.query, rec.Code, gotLimit, gotOffset, c.limit, c.offset)
		}
		var page struct {
			Data struct {
				Next string `json:"next_cursor"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || page.Data.Next != helpers.OffsetCursor(c.after) {
			t.Errorf("%q: the next page's cursor is %q (%v), want %q", c.query, page.Data.Next, err, helpers.OffsetCursor(c.after))
		}
	}
}
