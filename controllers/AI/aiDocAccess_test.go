package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	business "github.com/akashc777/OneCamp/business/AI"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// Docs incident, 14 Oct 2026 (D1). A member opening a workspace doc they can
// only read found Ask AI gone. Now the action decides the level
// (business/AI DocActionNeeds): reading needs view, changing needs edit, and
// the web offers by the same list (/ai/status doc_action_needs).

const askedDoc = "5c9b1e6d-e38a-4de8-84e8-4d628793c2cc"

// levelRules answers every doc at one level.
type levelRules struct {
	accessBusiness.Resolver
	d accessBusiness.Decision
}

func (l levelRules) Resolve(context.Context, accessBusiness.Subject, accessBusiness.Resource) (accessBusiness.Decision, error) {
	return l.d, nil
}

func decidingAs(t *testing.T, d accessBusiness.Decision) {
	t.Helper()
	was := accessBusiness.Rules
	accessBusiness.Rules = levelRules{Resolver: was, d: d}
	t.Cleanup(func() { accessBusiness.Rules = was })
}

func askDocAI(t *testing.T, action string) (int, string) {
	t.Helper()
	body := map[string]any{"action": action, "text": "We launch on Tuesday.", "doc_id": askedDoc}
	w := httptest.NewRecorder()
	DocAIComplete(w, docAIRequest(t, "/ai/doc/complete", body))
	var out struct {
		Msg string `json:"msg"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out.Msg
}

func TestAViewerCanAskWhatReadsTheDoc(t *testing.T) {
	useRuntime(t, fakeRuntime{model: &streamModel{chunks: []string{"Launch is Tuesday."}}})
	decidingAs(t, accessBusiness.Decision{Level: accessBusiness.LevelView, Reasons: []string{"workspace"}})
	// Explain reads only the text sent; a summary of a doc reads the doc
	// itself (the graph), which the integration matrix covers.
	if code, msg := askDocAI(t, "explain"); code != http.StatusOK {
		t.Errorf("a viewer's explain answered %d %q, want 200", code, msg)
	}
	if business.DocActionNeeds("summarize") != accessBusiness.LevelView {
		t.Error("a summary needs more than view")
	}
	// A viewer's change is refused (business/AI TestDocAIActionsNeedTheirLevel;
	// the 403 the handler answers is in the integration matrix: a refusal is
	// recorded in the audit, which needs the database).
}

func TestAnEditorMayRewrite(t *testing.T) {
	useRuntime(t, fakeRuntime{model: &streamModel{chunks: []string{"Better."}}})
	decidingAs(t, accessBusiness.Decision{Level: accessBusiness.LevelEdit})
	if code, msg := askDocAI(t, "improve"); code != http.StatusOK {
		t.Errorf("an editor's improve answered %d %q", code, msg)
	}
}

// The web reads the list it offers by from /ai/status, the one the server
// refuses by, whether or not a model is set up.
func TestAIStatusSaysWhatEachDocActionNeeds(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/ai/status", nil)
	r = r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, userModels.UserInfo{}))
	w := httptest.NewRecorder()
	GetAIStatus(w, r)
	var out struct {
		Data struct {
			Needs map[string]string `json:"doc_action_needs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	want := business.DocActionNeedsByName()
	if len(out.Data.Needs) != len(want) || out.Data.Needs["summarize"] != "view" || out.Data.Needs["improve"] != "edit" {
		t.Fatalf("doc_action_needs = %v, want %v", out.Data.Needs, want)
	}
}
