package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

var sam = userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "5f1c2b9e-0a4d-4e8b-9c3f-6d7e8f9a0b1c"}}

func post(t *testing.T, body string, signedIn bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/query/parse", strings.NewReader(body))
	if signedIn {
		r = r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, sam))
	}
	rec := httptest.NewRecorder()
	ParseQuery(rec, r)
	return rec
}

func allowAll(t *testing.T) {
	saved := allowParse
	t.Cleanup(func() { allowParse = saved })
	allowParse = func(context.Context, string) (bool, int) { return true, 0 }
}

// The parse endpoint answers a signed-in person's text: the spec, whether it
// may run, its issues with their places, its tokens and its canonical text.
func TestParseAnswersTheTextReadForThePerson(t *testing.T) {
	allowAll(t)
	rec := post(t, `{"text": "launch  type:issue -draft", "caret": 2}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got struct {
		Data struct {
			Spec struct {
				Scope struct {
					Kind string `json:"kind"`
				} `json:"scope"`
				Filter struct {
					Op      string `json:"op"`
					Filters []struct {
						Field    string `json:"field"`
						Operator string `json:"operator"`
					} `json:"filters"`
				} `json:"filter"`
			} `json:"spec"`
			Runs        bool              `json:"runs"`
			Issues      []json.RawMessage `json:"issues"`
			Tokens      []json.RawMessage `json:"tokens"`
			Text        string            `json:"text"`
			Suggestions []json.RawMessage `json:"suggestions"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	d := got.Data
	if d.Spec.Scope.Kind != "search" || d.Spec.Filter.Op != "and" || len(d.Spec.Filter.Filters) != 3 {
		t.Errorf("spec: %+v", d.Spec)
	}
	if f := d.Spec.Filter.Filters[1]; f.Field != "type" || f.Operator != "is" {
		t.Errorf("type:issue: %+v", f)
	}
	if !d.Runs || d.Issues == nil || len(d.Issues) != 0 || len(d.Tokens) != 6 {
		t.Errorf("runs %v, issues %v, tokens %d", d.Runs, d.Issues, len(d.Tokens))
	}
	if d.Text != "launch type:task -draft" {
		t.Errorf("canonical text %q", d.Text)
	}
	if d.Suggestions != nil {
		t.Errorf("no field starts with la: %s", d.Suggestions)
	}

	// A list refuses what it can't read, at its place, with what to do.
	rec = post(t, `{"text": "stauts = done", "scope": {"kind": "mine"}, "mode": "list"}`, true)
	var refused struct {
		Data struct {
			Runs   bool `json:"runs"`
			Issues []struct {
				Code    string `json:"code"`
				Span    [2]int `json:"span"`
				Level   string `json:"level"`
				Message string `json:"message"`
			} `json:"issues"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &refused); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("%d %v %s", rec.Code, err, rec.Body)
	}
	if is := refused.Data.Issues; refused.Data.Runs || len(is) != 1 || is[0].Code != "unknown_field" || is[0].Span != [2]int{0, 13} || is[0].Level != "error" || is[0].Message == "" {
		t.Errorf("a list's unknown field: %+v", refused.Data)
	}
}

// What can't be asked is said: nobody signed in, a body that isn't JSON, a
// list nobody knows, a project or table not named, a mode nobody knows, and
// too many reads in a minute.
func TestParseRefusesWhatItCantRead(t *testing.T) {
	allowAll(t)
	for name, c := range map[string]struct {
		body     string
		signedIn bool
		status   int
	}{
		"nobody signed in":   {`{"text": "a"}`, false, http.StatusUnauthorized},
		"not JSON":           {`text=a`, true, http.StatusBadRequest},
		"an unknown list":    {`{"text": "a", "scope": {"kind": "everything"}}`, true, http.StatusBadRequest},
		"a project, unnamed": {`{"text": "a", "scope": {"kind": "project"}}`, true, http.StatusBadRequest},
		"a table, unnamed":   {`{"text": "a", "scope": {"kind": "table"}}`, true, http.StatusBadRequest},
		"an unknown mode":    {`{"text": "a", "mode": "loose"}`, true, http.StatusBadRequest},
	} {
		rec := post(t, c.body, c.signedIn)
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), `"msg"`) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	allowParse = func(context.Context, string) (bool, int) { return false, 12 }
	rec := post(t, `{"text": "a"}`, true)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "12" || !strings.Contains(rec.Body.String(), "rate_limited") {
		t.Errorf("over the limit: %d %q %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
	}
}
