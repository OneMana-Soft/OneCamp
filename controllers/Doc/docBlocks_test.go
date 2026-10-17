package docController

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	business "github.com/akashc777/OneCamp/business/Doc"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/services/collab"
)

// appendWriter records what is appended, and answers err when set.
type appendWriter struct {
	collab.Writer
	docs  []string
	htmls []string
	by    []collab.By
	err   error
}

func (a *appendWriter) Append(_ context.Context, docUUID, html string, o collab.Options) (collab.Result, error) {
	if a.err != nil {
		return collab.Result{}, a.err
	}
	a.docs = append(a.docs, docUUID)
	a.htmls = append(a.htmls, html)
	a.by = append(a.by, o.By)
	return collab.Result{Blocks: 2, Persisted: true}, nil
}

const target = "3f6c2a1e-8d4b-4c7a-9e2f-1a2b3c4d5e6f"

func askAppend(t *testing.T, rules accessBusiness.Resolver, live *appendWriter, body string) (int, map[string]any) {
	t.Helper()
	oldRules, oldLive := accessBusiness.Rules, collab.Live
	accessBusiness.Rules, collab.Live = rules, live
	t.Cleanup(func() { accessBusiness.Rules, collab.Live = oldRules, oldLive })
	r := httptest.NewRequest(http.MethodPost, "/doc/blocks/append", strings.NewReader(body))
	who := userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "u-1"}}
	r = r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, who))
	w := httptest.NewRecorder()
	AppendDocBlocks(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

const twoBlocks = `{"doc_uuid":"` + target + `","content":{"type":"doc","content":[` +
	`{"type":"paragraph","attrs":{"blockId":"b-1","indent":1},"content":[{"type":"text","text":"Ship on Thursday"}]},` +
	`{"type":"paragraph","attrs":{"blockId":"b-2"},"content":[{"type":"text","text":"a link","marks":[{"type":"link","attrs":{"href":"javascript:alert(1)"}}]}]}]}}`

// Blocks moved in land at the doc's end, written by whoever moved them: with
// their indent, as new blocks (no ids), and never with an unsafe link.
func TestBlocksMovedInAreAppendedLive(t *testing.T) {
	live := &appendWriter{}
	code, body := askAppend(t, fixedRules{d: accessBusiness.Decision{Level: accessBusiness.LevelEdit}}, live, twoBlocks)
	if code != http.StatusOK {
		t.Fatalf("answered %d %v, want 200", code, body)
	}
	if len(live.docs) != 1 || live.docs[0] != target || live.by[0].UserUUID != "u-1" {
		t.Fatalf("appended %v by %v", live.docs, live.by)
	}
	html := live.htmls[0]
	for _, want := range []string{`<p data-indent="1">Ship on Thursday</p>`, "a link"} {
		if !strings.Contains(html, want) {
			t.Errorf("the appended HTML %q lacks %q", html, want)
		}
	}
	for _, never := range []string{"javascript:", "data-block-id", "b-1"} {
		if strings.Contains(html, never) {
			t.Errorf("the appended HTML %q holds %q", html, never)
		}
	}
	if data, _ := body["data"].(map[string]any); data["blocks"] != float64(2) {
		t.Errorf("answered %v", body)
	}
}

// Only someone who can edit the doc moves blocks into it, and a doc in the
// trash takes none; nothing is written for them.
func TestBlocksGoOnlyWhereThePersonCanEdit(t *testing.T) {
	cases := []struct {
		name  string
		rules fixedRules
		want  int
	}{
		{"a reader", fixedRules{d: accessBusiness.Decision{Level: accessBusiness.LevelView}}, http.StatusForbidden},
		{"a commenter", fixedRules{d: accessBusiness.Decision{Level: accessBusiness.LevelComment}}, http.StatusForbidden},
		{"no such doc", fixedRules{err: business.ErrNotFound}, http.StatusNotFound},
		{"a doc in the trash", fixedRules{d: accessBusiness.Decision{Level: accessBusiness.LevelEdit, Deleted: true}}, http.StatusGone},
	}
	for _, c := range cases {
		live := &appendWriter{}
		if code, _ := askAppend(t, c.rules, live, twoBlocks); code != c.want {
			t.Errorf("%s: answered %d, want %d", c.name, code, c.want)
		}
		if len(live.docs) != 0 {
			t.Errorf("%s: appended %v", c.name, live.docs)
		}
	}
}

func TestBlocksMovedInNeedBlocksAndADoc(t *testing.T) {
	edit := fixedRules{d: accessBusiness.Decision{Level: accessBusiness.LevelEdit}}
	for name, body := range map[string]string{
		"no doc":       `{"content":{"type":"doc","content":[]}}`,
		"no content":   `{"doc_uuid":"` + target + `"}`,
		"empty blocks": `{"doc_uuid":"` + target + `","content":{"type":"doc","content":[{"type":"paragraph"}]}}`,
		"not JSON":     `{`,
	} {
		live := &appendWriter{}
		if code, _ := askAppend(t, edit, live, body); code != http.StatusBadRequest {
			t.Errorf("%s: answered %d, want 400", name, code)
		}
		if len(live.docs) != 0 {
			t.Errorf("%s: appended %v", name, live.docs)
		}
	}
}

// A write the collaboration service can't make says so, with what to do.
func TestBlocksMovedInWhenTheLiveDocIsDown(t *testing.T) {
	live := &appendWriter{err: collab.ErrUnavailable}
	code, body := askAppend(t, fixedRules{d: accessBusiness.Decision{Level: accessBusiness.LevelEdit}}, live, twoBlocks)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("answered %d, want 503", code)
	}
	if msg, _ := body["msg"].(string); !strings.Contains(msg, "Try again") {
		t.Errorf("said %q", msg)
	}
}
