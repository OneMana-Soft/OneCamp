package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	business "github.com/akashc777/OneCamp/business/Publish"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

func copyAs(body string, signedIn bool) (int, map[string]any) {
	r := httptest.NewRequest(http.MethodPost, "/guest/links/copy", strings.NewReader(body))
	if signedIn {
		who := userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "a0000000-0000-4000-8000-000000000001"}}
		r = r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, who))
	}
	w := httptest.NewRecorder()
	Duplicate(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// A member copies a page by its address or its link's token; each refusal
// reads as what to do next.
func TestCopyingAPage(t *testing.T) {
	old := duplicate
	t.Cleanup(func() { duplicate = old })
	var got business.Ref
	var gotK string
	duplicate = func(_ context.Context, by *dgraphStruct.DgraphUser, agent string, ref business.Ref, k string) (string, error) {
		got, gotK = ref, k
		if by.Uuid != "a0000000-0000-4000-8000-000000000001" || agent != "" {
			t.Errorf("by %+v, agent %q", by, agent)
		}
		return "copy-1", nil
	}
	if code, out := copyAs(`{"slug":"launch","k":"3.99.sig"}`, true); code != http.StatusOK || out["doc_uuid"] != "copy-1" || got.Slug != "launch" || gotK != "3.99.sig" {
		t.Errorf("by address: %d %v %+v %q", code, out, got, gotK)
	}
	if code, _ := copyAs(`{"token":"tok"}`, true); code != http.StatusOK || got.Token != "tok" {
		t.Errorf("by token: %d %+v", code, got)
	}
	if code, _ := copyAs(`{}`, true); code != http.StatusBadRequest {
		t.Errorf("nothing named: %d", code)
	}
	if code, _ := copyAs(`{"slug":"x"}`, false); code != http.StatusUnauthorized {
		t.Errorf("no one signed in: %d", code)
	}
	for err, want := range map[error]int{
		business.ErrNoCopies: http.StatusForbidden, business.ErrLocked: http.StatusForbidden, business.ErrNotFound: http.StatusNotFound,
		business.ErrGone: http.StatusGone, business.ErrBusy: http.StatusTooManyRequests, business.ErrForbidden: http.StatusForbidden,
	} {
		err := err
		duplicate = func(context.Context, *dgraphStruct.DgraphUser, string, business.Ref, string) (string, error) {
			return "", err
		}
		if code, out := copyAs(`{"slug":"launch"}`, true); code != want || out["msg"] == "" {
			t.Errorf("%v: %d %v", err, code, out)
		}
	}
}
