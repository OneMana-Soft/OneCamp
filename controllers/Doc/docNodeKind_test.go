package docController

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	business "github.com/akashc777/OneCamp/business/Doc"
	treeBusiness "github.com/akashc777/OneCamp/business/Tree"
	unfurlBusiness "github.com/akashc777/OneCamp/business/Unfurl"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// Only a page opens live (docs F1): the collaboration service's session, and
// the older authorisation a service that predates it still asks, refuse a
// folder, a file or a link with 409, whatever the person may do with it, so
// none is ever opened in the editor.
func TestTheLiveSessionRefusesANodeThatIsntAPage(t *testing.T) {
	for _, kind := range []string{"folder", "file", "link"} {
		code, body := askSession(t, fixedRules{d: accessBusiness.Decision{Level: accessBusiness.LevelFull, Owner: true, NodeKind: kind}})
		if code != http.StatusConflict || body["code"] != "not_a_page" {
			t.Errorf("a %s's session: %d %v, want 409 not_a_page", kind, code, body)
		}
		old := accessBusiness.Rules
		accessBusiness.Rules = fixedRules{d: accessBusiness.Decision{Level: accessBusiness.LevelFull, Owner: true, NodeKind: kind}}
		r := httptest.NewRequest(http.MethodPost, "/docColab/authorize", strings.NewReader(`{"doc_uuid":"5f0c4a2e-8a63-4f3e-9d2b-1c7a9e0b3d41"}`))
		who := userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "u-1"}}
		r = r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, who))
		w := httptest.NewRecorder()
		AuthorizeDocSocket(w, r)
		accessBusiness.Rules = old
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"not_a_page"`) {
			t.Errorf("a %s's socket: %d %s, want 409 not_a_page", kind, w.Code, w.Body)
		}
	}
	if code, _ := askSession(t, fixedRules{d: accessBusiness.Decision{Level: accessBusiness.LevelEdit}}); code != http.StatusOK {
		t.Errorf("a page's session: %d", code)
	}
}

// A tree row says what the node is (the F1a/F1b contract, 2): its kind,
// colour and link; a page says none of them.
func TestTreeRowsSayWhatTheNodeIs(t *testing.T) {
	raw, err := json.Marshal(treeNodes([]treeBusiness.Node{
		{Kind: "doc", UUID: "a", Title: "Roadmap", Data: &dgraphStruct.DgraphDoc{Uuid: "a", NodeKind: "link", Color: "sky", LinkURL: "https://docs.google.com/d/1"}},
		{Kind: "doc", UUID: "b", Title: "Plan", Data: &dgraphStruct.DgraphDoc{Uuid: "b"}},
		{Kind: "doc", UUID: "c", Title: "msa.pdf", Data: &dgraphStruct.DgraphDoc{Uuid: "c", NodeKind: "file"}},
	}, map[string]*business.FileFacts{"c": {Name: "msa.pdf", SizeBytes: 48213, MIME: "application/pdf", Versions: 3, AttachmentID: "att-1"}}))
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if rows[0]["node_kind"] != "link" || rows[0]["color"] != "sky" || rows[0]["link_url"] != "https://docs.google.com/d/1" || rows[0]["kind"] != "doc" {
		t.Errorf("a link's row: %v", rows[0])
	}
	for _, k := range []string{"node_kind", "color", "link_url", "file"} {
		if _, ok := rows[1][k]; ok {
			t.Errorf("a page's row carries %s: %v", k, rows[1])
		}
	}
	file, _ := rows[2]["file"].(map[string]any)
	if rows[2]["node_kind"] != "file" || file["name"] != "msa.pdf" || file["size_bytes"] != float64(48213) || file["mime"] != "application/pdf" ||
		file["versions"] != float64(3) || file["attachment_id"] != "att-1" {
		t.Errorf("a file's row: %v", rows[2])
	}
}

// A list row says what the node is too (the contract, 2), under the list's
// names: doc_node_kind, doc_color and doc_link_url.
func TestListRowsSayWhatTheNodeIs(t *testing.T) {
	old := accessBusiness.Rules
	accessBusiness.Rules = fixedRules{d: accessBusiness.Decision{Level: accessBusiness.LevelView}}
	t.Cleanup(func() { accessBusiness.Rules = old })
	items, err := listItems(context.Background(), accessBusiness.Subject{UserUID: "0x1", UserUUID: "u-1"}, []*dgraphStruct.DgraphDoc{
		{Uuid: "f", Title: "Contracts", NodeKind: "folder", Color: "moss"},
	})
	if err != nil || len(items) != 1 {
		t.Fatalf("%+v %v", items, err)
	}
	raw, _ := json.Marshal(items[0])
	var row map[string]any
	_ = json.Unmarshal(raw, &row)
	if row["doc_node_kind"] != "folder" || row["doc_color"] != "moss" {
		t.Errorf("a folder's list row: %s", raw)
	}
}

// Only a page or a folder holds children (D-30): putting a page under a file
// or a link is refused with 409, in words, and the cap on a subtree's size
// names files too, which it counts.
func TestAFileOrALinkHoldsNothing(t *testing.T) {
	w := httptest.NewRecorder()
	treeFailed(w, httptest.NewRequest(http.MethodPost, "/doc/move", nil), "MoveDoc", errors.Join(errors.New("graph"), treeBusiness.ErrLeaf))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"cannot_hold_items"`) {
		t.Errorf("a move under a file: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	tooManyPages(w)
	if !strings.Contains(w.Body.String(), "pages and files") {
		t.Errorf("the cap says %s", w.Body)
	}
}

// A folder or a link is made through POST /doc/createDoc (the F1a/F1b
// contract 8): node_kind "folder" or "link" and nothing else (a file comes
// through POST /doc/files); a link's address is a public https one, its title
// the address until its preview names it, which is fetched after the node is
// made; a template is a page's.
func TestAFolderOrALinkIsMadeThroughCreateDoc(t *testing.T) {
	prevMake, prevName, prevRules := makeDoc, nameLinkLater, accessBusiness.Rules
	t.Cleanup(func() { makeDoc, nameLinkLater, accessBusiness.Rules = prevMake, prevName, prevRules })
	accessBusiness.Rules = fixedRules{d: accessBusiness.Decision{Level: accessBusiness.LevelEdit}}
	var made []business.NewDoc
	makeDoc = func(_ context.Context, _ *dgraphStruct.DgraphUser, in business.NewDoc, _ *dgraphStruct.DgraphDoc) (*dgraphStruct.DgraphDoc, error) {
		made = append(made, in)
		return &dgraphStruct.DgraphDoc{Uuid: "5f0c4a2e-8a63-4f3e-9d2b-1c7a9e0b3d41", Title: in.Title, NodeKind: in.NodeKind, LinkURL: in.LinkURL}, nil
	}
	var named []string
	nameLinkLater = func(_ context.Context, _, docUUID, address string) { named = append(named, docUUID+" "+address) }
	ask := func(body string) int {
		r := httptest.NewRequest(http.MethodPost, "/doc/createDoc", strings.NewReader(body))
		who := userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "b0b00000-0000-4000-8000-000000000002"}}
		r = r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, who))
		w := httptest.NewRecorder()
		CreateDocInTree(w, r)
		return w.Code
	}
	if code := ask(`{"doc_title":"Contracts","node_kind":"folder","space":"workspace"}`); code != http.StatusOK || made[0].NodeKind != "folder" {
		t.Fatalf("a folder: %d %+v", code, made)
	}
	if code := ask(`{"node_kind":"link","link_url":"https://docs.google.com/d/1","space":"workspace"}`); code != http.StatusOK ||
		made[1].NodeKind != "link" || made[1].Title != "https://docs.google.com/d/1" || made[1].LinkURL != "https://docs.google.com/d/1" {
		t.Fatalf("a link: %d %+v", code, made)
	}
	if len(named) != 1 || named[0] != "5f0c4a2e-8a63-4f3e-9d2b-1c7a9e0b3d41 https://docs.google.com/d/1" {
		t.Errorf("the link's preview asked for %v", named)
	}
	for name, body := range map[string]string{
		"a file":                `{"node_kind":"file"}`,
		"a page by name":        `{"node_kind":"page"}`,
		"an unknown kind":       `{"node_kind":"drive"}`,
		"a link over http":      `{"node_kind":"link","link_url":"http://example.com"}`,
		"a link with none":      `{"node_kind":"link"}`,
		"an address on a page":  `{"link_url":"https://example.com"}`,
		"a folder's template":   `{"node_kind":"folder","template_id":"t1"}`,
		"javascript: as a link": `{"node_kind":"link","link_url":"javascript:alert(1)"}`,
	} {
		if code := ask(body); code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, code)
		}
	}
	if len(made) != 2 {
		t.Errorf("a refused node was made: %+v", made[2:])
	}
}

// A link is named after its preview: its page's title, else its site's name;
// a preview that says neither leaves it called by its address.
func TestALinksTitleComesFromItsPreview(t *testing.T) {
	for _, c := range []struct {
		card unfurlBusiness.Card
		want string
	}{
		{unfurlBusiness.Card{Title: "  Q3 roadmap ", SiteName: "Google Docs", Fetched: true}, "Q3 roadmap"},
		{unfurlBusiness.Card{SiteName: "drive.google.com", Fetched: true}, "drive.google.com"},
		{unfurlBusiness.Card{Fetched: false, Reason: "sign_in"}, ""},
		{unfurlBusiness.Card{Title: strings.Repeat("a", 400), Fetched: true}, strings.Repeat("a", 255)},
	} {
		if got := linkTitle(c.card); got != c.want {
			t.Errorf("%+v: %q", c.card, got)
		}
	}
}
