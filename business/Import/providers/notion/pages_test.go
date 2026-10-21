package notion

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"testing"

	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	notionAdapter "github.com/akashc777/OneCamp/business/ContentIO/adapters/notion"
	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/helpers/safefetch"
	dataTableModels "github.com/akashc777/OneCamp/models/postgres/DataTable"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// fakeNotion answers the API's requests from canned answers, by method and
// path (with its query), as Notion documents them (pages.go).
type fakeNotion struct {
	t       *testing.T
	answers map[string]string
	asked   []string
}

func (f *fakeNotion) RoundTrip(r *http.Request) (*http.Response, error) {
	key := r.Method + " " + strings.TrimPrefix(r.URL.RequestURI(), "/v1")
	f.asked = append(f.asked, key)
	if v := r.Header.Get("Notion-Version"); v != pagesVersion {
		f.t.Errorf("%s asked at Notion-Version %q", key, v)
	}
	body, ok := f.answers[key]
	status := http.StatusOK
	if !ok {
		status, body = http.StatusNotFound, `{"object":"error","status":404,"code":"object_not_found"}`
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

type noWait struct{}

func (noWait) Wait(context.Context) error { return nil }

// withFakeNotion points the provider at answers, and the seams at fakes: the
// id map holds tables, and every table made is kept.
func withFakeNotion(t *testing.T, answers map[string]string) (*Provider, *fakeNotion, *[]notionAdapter.Database) {
	t.Helper()
	fake := &fakeNotion{t: t, answers: answers}
	saved := importProvider.SharedAPIClient
	savedFetch, savedFile, savedTable, savedLookup, savedRecord := fetchPicture, fetchFile, tableFromDatabase, lookupMapped, recordMapped
	t.Cleanup(func() {
		importProvider.SharedAPIClient = saved
		fetchPicture, fetchFile, tableFromDatabase, lookupMapped, recordMapped = savedFetch, savedFile, savedTable, savedLookup, savedRecord
	})
	fetchFile = func(_ context.Context, address string) (*safefetch.Response, error) {
		u, _ := url.Parse(address)
		return &safefetch.Response{URL: u, Status: http.StatusOK, ContentType: "application/octet-stream", Body: []byte("file " + path.Base(u.Path))}, nil
	}
	importProvider.SharedAPIClient = &http.Client{Transport: fake}
	mapped := map[string]uuid.UUID{}
	lookupMapped = func(_ context.Context, _ uuid.UUID, entity, source string) (uuid.UUID, error) {
		return mapped[entity+":"+source], nil
	}
	recordMapped = func(_ context.Context, _ uuid.UUID, entity, source string, id uuid.UUID, _ *string, _ json.RawMessage, owned bool) error {
		if !owned {
			t.Errorf("a table the import made was recorded as not its own")
		}
		mapped[entity+":"+source] = id
		return nil
	}
	var made []notionAdapter.Database
	tableFromDatabase = func(_ context.Context, _ dataTableBusiness.Actor, db notionAdapter.Database) (*dataTableModels.DataTable, int, error) {
		made = append(made, db)
		return &dataTableModels.DataTable{Id: uuid.MustParse("00000000-0000-4000-8000-0000000000aa"), Name: db.Title}, 0, nil
	}
	fetchPicture = func(_ context.Context, address string) (*safefetch.Response, error) {
		u, _ := url.Parse(address)
		return &safefetch.Response{URL: u, Status: http.StatusOK, ContentType: "image/png", Body: []byte("\x89PNG picture")}, nil
	}
	return &Provider{rl: noWait{}, dbCache: map[uuid.UUID][]string{}}, fake, &made
}

const (
	rootID   = "11111111111111111111111111111111"
	runbooks = "22222222-2222-2222-2222-222222222222"
	deploys  = "33333333333333333333333333333333"
	dbID     = "44444444-4444-4444-4444-444444444444"
	dsID     = "55555555-5555-5555-5555-555555555555"
)

func page(id, title, icon string) string {
	ic := "null"
	if icon != "" {
		ic = `{"type":"emoji","emoji":"` + icon + `"}`
	}
	return `{"object":"page","id":"` + id + `","in_trash":false,"icon":` + ic + `,"parent":{"type":"workspace","workspace":true},
		"properties":{"title":{"id":"title","type":"title","title":[{"type":"text","plain_text":"` + title + `"}]}}}`
}

func children(blocks ...string) string {
	return `{"object":"list","results":[` + strings.Join(blocks, ",") + `],"has_more":false,"next_cursor":null}`
}

func text(s string) string { return `{"type":"text","plain_text":"` + s + `","annotations":{}}` }

// A three-level workspace as Notion's API gives it: a root page with blocks of
// most kinds and a database, a page under it inside a column, and a page under
// that.
func treeAnswers() map[string]string {
	return map[string]string{
		"GET /pages/" + rootID: page(rootID, "Engineering", "🛠️"),
		"GET /blocks/" + rootID + "/children?page_size=100": children(
			`{"id":"b1","type":"heading_1","heading_1":{"rich_text":[`+text("Overview")+`],"color":"blue","is_toggleable":false}}`,
			`{"id":"b2","type":"paragraph","paragraph":{"rich_text":[{"type":"text","plain_text":"Ship ","annotations":{"bold":true}},{"type":"text","plain_text":"safely","annotations":{"color":"red_background"},"text":{"content":"safely","link":{"url":"/`+deploys+`"}}}]}}`,
			`{"id":"b3","type":"bulleted_list_item","bulleted_list_item":{"rich_text":[`+text("One")+`]}}`,
			`{"id":"b4","type":"bulleted_list_item","bulleted_list_item":{"rich_text":[`+text("Two")+`]}}`,
			`{"id":"b5","type":"to_do","to_do":{"rich_text":[`+text("Tag it")+`],"checked":true}}`,
			`{"id":"b6","type":"toggle","has_children":true,"toggle":{"rich_text":[`+text("Rollback")+`]}}`,
			`{"id":"b7","type":"callout","callout":{"rich_text":[`+text("Never on Fridays")+`],"icon":{"type":"emoji","emoji":"⚠️"},"color":"blue_background"}}`,
			`{"id":"b8","type":"code","code":{"rich_text":[`+text("make deploy")+`],"language":"shell"}}`,
			`{"id":"b9","type":"equation","equation":{"expression":"e=mc^2"}}`,
			`{"id":"ba","type":"image","image":{"type":"file","file":{"url":"https://prod-files-secure.s3.us-west-2.amazonaws.com/x/diagram.png?X-Amz-Signature=1","expiry_time":"2026-10-14T10:00:00.000Z"},"caption":[]}}`,
			`{"id":"bb","type":"image","image":{"type":"external","external":{"url":"https://example.com/logo.png"},"caption":[`+text("Logo")+`]}}`,
			`{"id":"bc","type":"divider","divider":{}}`,
			`{"id":"bd","type":"table","has_children":true,"table":{"table_width":2,"has_column_header":true,"has_row_header":false}}`,
			`{"id":"`+dbID+`","type":"child_database","child_database":{"title":"Releases"}}`,
			`{"id":"be","type":"column_list","has_children":true,"column_list":{}}`,
			`{"id":"bf","type":"breadcrumb","breadcrumb":{}}`,
			`{"id":"bg","type":"file","file":{"type":"file","file":{"url":"https://prod-files-secure.s3.us-west-2.amazonaws.com/x/plan.pdf"},"name":"plan.pdf","caption":[]}}`,
		),
		"GET /blocks/b6/children?page_size=100": children(`{"id":"b6a","type":"paragraph","paragraph":{"rich_text":[` + text("Revert the tag") + `]}}`),
		"GET /blocks/bd/children?page_size=100": children(
			`{"id":"r1","type":"table_row","table_row":{"cells":[[`+text("Step")+`],[`+text("Owner")+`]]}}`,
			`{"id":"r2","type":"table_row","table_row":{"cells":[[`+text("Build")+`],[`+text("Ada")+`]]}}`,
		),
		"GET /blocks/be/children?page_size=100":   children(`{"id":"col1","type":"column","has_children":true,"column":{}}`),
		"GET /blocks/col1/children?page_size=100": children(`{"id":"` + runbooks + `","type":"child_page","child_page":{"title":"Runbooks"}}`),
		"GET /databases/" + dbID:                  `{"object":"database","id":"` + dbID + `","title":[` + text("Releases") + `],"icon":{"type":"emoji","emoji":"🚀"},"in_trash":false,"data_sources":[{"id":"` + dsID + `","name":"Releases"}]}`,
		"GET /data_sources/" + dsID: `{"object":"data_source","id":"` + dsID + `","properties":{
			"Name":{"id":"title","name":"Name","type":"title","title":{}},
			"Stage":{"id":"st","name":"Stage","type":"status","status":{"options":[{"id":"o1","name":"Planned"},{"id":"o2","name":"Shipped"}]}},
			"Due":{"id":"du","name":"Due","type":"date","date":{}}}}`,
		"POST /data_sources/" + dsID + "/query": `{"object":"list","results":[
			{"object":"page","id":"row1","properties":{"Name":{"type":"title","title":[` + text("v2.73") + `]},"Stage":{"type":"status","status":{"name":"Shipped"}},"Due":{"type":"date","date":{"start":"2026-10-20T09:00:00.000+05:30"}}}},
			{"object":"page","id":"row2","properties":{"Name":{"type":"title","title":[` + text("v2.74") + `]},"Stage":{"type":"status","status":{"name":"Planned"}},"Due":{"type":"date","date":null}}}],"has_more":false,"next_cursor":null}`,
		"GET /pages/" + strings.ReplaceAll(runbooks, "-", ""): page(runbooks, "Runbooks", ""),
		"GET /blocks/" + strings.ReplaceAll(runbooks, "-", "") + "/children?page_size=100": children(
			`{"id":"` + deploys + `","type":"child_page","child_page":{"title":"Deploys"}}`,
		),
		"GET /pages/" + deploys: page(deploys, "Deploys", "🚀"),
		"GET /blocks/" + deploys + "/children?page_size=100": children(
			`{"id":"d1","type":"paragraph","paragraph":{"rich_text":[{"type":"mention","plain_text":"Engineering","mention":{"type":"page","page":{"id":"` + rootID + `"}}}]}}`,
		),
	}
}

// structure.md P2.4: a three-level workspace keeps its tree, titles and
// icons, a database in a page becomes a table shown where it stood, and no
// block becomes a marker of something left out.
func TestATreeOfNotionPagesComesAcrossWithItsTitlesIconsAndTables(t *testing.T) {
	p, _, made := withFakeNotion(t, treeAnswers())
	job := &importModels.Job{Id: uuid.New(), TriggeredBy: ptrUUID(uuid.New())}
	tr := &tree{p: p, ctx: context.Background(), tok: "secret", job: job, titles: map[string]string{}, tables: map[string][]string{}}
	if err := tr.page(rootID, ""); err != nil {
		t.Fatal(err)
	}
	if len(tr.pages) != 3 {
		t.Fatalf("pages %+v", tr.pages)
	}
	root, rb, dp := tr.pages[0], tr.pages[1], tr.pages[2]
	if root.title != "Engineering" || root.icon != "🛠️" || root.parent != "" ||
		rb.title != "Runbooks" || rb.parent != rootID || dp.title != "Deploys" || dp.icon != "🚀" || dp.parent != pageKey(runbooks) {
		t.Fatalf("tree %+v / %+v / %+v", root, rb, dp)
	}

	html, err := tr.html(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(*made) != 1 {
		t.Fatalf("tables made %+v", *made)
	}
	db := (*made)[0]
	if db.Title != "Releases" || db.Icon != "🚀" || !db.Private || len(db.Columns) != 3 || db.Columns[0].Name != "Name" ||
		len(db.Rows) != 2 || db.Rows[0]["Stage"] != "Shipped" || db.Rows[0]["Due"] != "2026-10-20" {
		t.Fatalf("the database as a table: %+v", db)
	}
	if len(root.files) != 2 || string(root.files["files/ba.png"]) != "\x89PNG picture" || !strings.Contains(html, `<img src="files/ba.png">`) ||
		string(root.files["files/bg.pdf"]) != "file plan.pdf" || !strings.Contains(html, `data-src="files/bg.pdf"`) {
		t.Fatalf("pictures and files read from Notion: %v", root.files)
	}
	for _, unwanted := range []string{"notion-unsupported", "[Notion", "breadcrumb"} {
		if strings.Contains(html, unwanted) {
			t.Errorf("%q in the page:\n%s", unwanted, html)
		}
	}

	// What the docs pipeline reads from it (business/Import pages_worker.go
	// reads the HTML as foreign HTML).
	body, err := contentcodec.FromHTML(html, contentcodec.HTMLOptions{Foreign: true, Assets: true})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	var links []string
	body.Walk(func(n *contentcodec.Node) bool {
		kinds[n.Type] = true
		for _, m := range n.Marks {
			if m.Type == "link" {
				links = append(links, m.Attrs["href"].(string))
			}
		}
		if n.Type == "callout" && (n.Attrs["emoji"] != "⚠️" || n.Attrs["color"] != "blue") {
			t.Errorf("callout %+v", n.Attrs)
		}
		if n.Type == "codeBlock" && n.Attrs["language"] != "shell" {
			t.Errorf("code block %+v", n.Attrs)
		}
		if n.Type == "tableEmbed" && n.Attrs["tableId"] != "00000000-0000-4000-8000-0000000000aa" {
			t.Errorf("table embed %+v", n.Attrs)
		}
		return true
	})
	for _, want := range []string{"heading", "bulletList", "taskList", "collapsible", "callout", "codeBlock", "blockMath", "image", "horizontalRule", "table", "tableHeader", "tableEmbed", "columnList", "fileBlock"} {
		if !kinds[want] {
			t.Errorf("no %s in the doc:\n%s", want, html)
		}
	}
	// The link to Deploys and the column's Runbooks both point at pages of the
	// tree, by key, for the pipeline to point at their docs.
	if strings.Join(links, " ") != deploys+" "+pageKey(runbooks) {
		t.Errorf("links %v", links)
	}

	// Deploys mentions Engineering, a page of the tree.
	dpHTML, err := tr.html(dp)
	if err != nil || !strings.Contains(dpHTML, `<a href="`+rootID+`">Engineering</a>`) {
		t.Fatalf("a page mention: %s, %v", dpHTML, err)
	}
}

// A picture Notion keeps is read with its page, before the pages under it:
// its address expires within the hour, and a large tree takes longer to read.
func TestAPictureIsReadWithItsPage(t *testing.T) {
	p, fake, _ := withFakeNotion(t, treeAnswers())
	fetchPicture = func(_ context.Context, address string) (*safefetch.Response, error) {
		fake.asked = append(fake.asked, "picture")
		u, _ := url.Parse(address)
		return &safefetch.Response{URL: u, Status: http.StatusOK, ContentType: "image/png", Body: []byte("\x89PNG picture")}, nil
	}
	tr := &tree{p: p, ctx: context.Background(), tok: "secret", job: &importModels.Job{Id: uuid.New(), TriggeredBy: ptrUUID(uuid.New())}, titles: map[string]string{}, tables: map[string][]string{}}
	if err := tr.page(rootID, ""); err != nil {
		t.Fatal(err)
	}
	picture, next := -1, -1
	for i, asked := range fake.asked {
		switch asked {
		case "picture":
			picture = i
		case "GET /pages/" + pageKey(runbooks):
			next = i
		}
	}
	if picture < 0 || next < 0 || picture > next {
		t.Fatalf("asked in this order: %v", fake.asked)
	}
}

// A tree read again (a retried chunk) finds the tables the import made, and
// makes none twice.
func TestATreeReadAgainMakesItsTablesOnce(t *testing.T) {
	p, _, made := withFakeNotion(t, treeAnswers())
	job := &importModels.Job{Id: uuid.New(), TriggeredBy: ptrUUID(uuid.New())}
	for i := 0; i < 2; i++ {
		tr := &tree{p: p, ctx: context.Background(), tok: "secret", job: job, titles: map[string]string{}, tables: map[string][]string{}}
		if err := tr.page(rootID, ""); err != nil {
			t.Fatal(err)
		}
		if len(tr.tables[dbID]) != 1 {
			t.Fatalf("read %d: tables %v", i, tr.tables)
		}
	}
	if len(*made) != 1 {
		t.Fatalf("made %d tables", len(*made))
	}
}

// Planning finds each tree of pages shared with the connection, answering
// while the admin waits: a page whose parent page isn't shared is a root, and
// one inside a column is taken for a root without asking which page the
// column is in; a database's rows and pages in the trash are no tree.
func TestPlanFindsEachTreeOfSharedPages(t *testing.T) {
	hit := func(id, parent string) string {
		return `{"object":"page","id":"` + id + `","in_trash":false,"parent":` + parent + `}`
	}
	p, fake, _ := withFakeNotion(t, map[string]string{
		"POST /search": `{"object":"list","results":[` + strings.Join([]string{
			hit("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", `{"type":"workspace","workspace":true}`),
			hit("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", `{"type":"page_id","page_id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}`),
			hit("cccccccc-cccc-cccc-cccc-cccccccccccc", `{"type":"block_id","block_id":"column-1"}`),
			hit("dddddddd-dddd-dddd-dddd-dddddddddddd", `{"type":"data_source_id","data_source_id":"ds"}`),
			hit("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee", `{"type":"page_id","page_id":"99999999-9999-9999-9999-999999999999"}`),
			`{"object":"page","id":"ffffffff-ffff-ffff-ffff-ffffffffffff","in_trash":true,"parent":{"type":"workspace","workspace":true}}`,
		}, ",") + `],"has_more":false,"next_cursor":null}`,
	})
	roots, count, more, err := p.planPages(context.Background(), "secret")
	if err != nil {
		t.Fatal(err)
	}
	if count != 4 || more || strings.Join(roots, ",") != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa,cccccccccccccccccccccccccccccccc,eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee" {
		t.Fatalf("roots %v of %d pages", roots, count)
	}
	if strings.Contains(strings.Join(fake.asked, " "), "/blocks/") {
		t.Fatalf("planning asked about blocks: %v", fake.asked)
	}
}

// A page planning took for a root because it sits in a column brings
// nothing when the page the column is in can be read, whose tree brings it,
// and comes across as a tree of its own when it can't.
func TestAPageInAColumnComesAcrossOnce(t *testing.T) {
	inColumn := `{"object":"page","id":"cccccccc-cccc-cccc-cccc-cccccccccccc","in_trash":false,"parent":{"type":"block_id","block_id":"column-1"},
		"properties":{"title":{"id":"title","type":"title","title":[{"type":"text","plain_text":"Notes"}]}}}`
	answers := map[string]string{
		"GET /pages/cccccccccccccccccccccccccccccccc": inColumn,
		"GET /blocks/column-1":                        `{"object":"block","id":"column-1","type":"column","parent":{"type":"block_id","block_id":"list-1"}}`,
		"GET /blocks/list-1":                          `{"object":"block","id":"list-1","type":"column_list","parent":{"type":"page_id","page_id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}}`,
		"GET /pages/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": page("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "Wiki", ""),
	}
	p, _, _ := withFakeNotion(t, answers)
	tr := &tree{p: p, ctx: context.Background(), tok: "secret", job: &importModels.Job{Id: uuid.New(), TriggeredBy: ptrUUID(uuid.New())}, titles: map[string]string{}, tables: map[string][]string{}}
	if elsewhere, err := tr.inAnotherTree("cccccccccccccccccccccccccccccccc"); err != nil || !elsewhere {
		t.Fatalf("in a column of a page the connection reads: %v, %v", elsewhere, err)
	}
	delete(answers, "GET /pages/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if elsewhere, err := tr.inAnotherTree("cccccccccccccccccccccccccccccccc"); err != nil || elsewhere {
		t.Fatalf("in a column of a page the connection can't read: %v, %v", elsewhere, err)
	}
}

// What a tree leaves in Notion is said to the importer, once each: a page the
// connection can't read, a tree past its size, and rows a table didn't take.
func TestWhatATreeLeavesBehindIsSaid(t *testing.T) {
	answers := treeAnswers()
	delete(answers, "GET /pages/"+deploys)
	p, _, _ := withFakeNotion(t, answers)
	var notes []string
	saved := noteImport
	t.Cleanup(func() { noteImport = saved })
	noteImport = func(_ context.Context, _ uuid.UUID, _ *uuid.UUID, _, source, _, code, _ string, _ json.RawMessage) {
		notes = append(notes, code+" "+source)
	}
	tableFromDatabase = func(_ context.Context, _ dataTableBusiness.Actor, db notionAdapter.Database) (*dataTableModels.DataTable, int, error) {
		return &dataTableModels.DataTable{Id: uuid.New(), Name: db.Title}, 1, nil
	}
	tr := &tree{p: p, ctx: context.Background(), tok: "secret", job: &importModels.Job{Id: uuid.New(), TriggeredBy: ptrUUID(uuid.New())}, titles: map[string]string{}, tables: map[string][]string{}}
	if err := tr.page(rootID, ""); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PAGE_LEFT_OUT " + deploys, "TABLE_ROWS_LEFT_OUT " + dsID} {
		if !slices.Contains(notes, want) {
			t.Errorf("no %q in %v", want, notes)
		}
	}

	savedMax := maxTreePages
	t.Cleanup(func() { maxTreePages = savedMax })
	maxTreePages, notes = 1, nil
	tr = &tree{p: p, ctx: context.Background(), tok: "secret", job: tr.job, titles: map[string]string{}, tables: map[string][]string{}}
	if err := tr.page(rootID, ""); err != nil {
		t.Fatal(err)
	}
	capped := 0
	for _, n := range notes {
		if strings.HasPrefix(n, "PAGES_LEFT_OUT") {
			capped++
		}
	}
	if len(tr.pages) != 1 || capped != 1 {
		t.Fatalf("%d pages, notes %v", len(tr.pages), notes)
	}
}

// A row's values come across as cells: text, numbers, yes or no, days and
// names, a formula as what it works out to.
func TestARowsValuesComeAcrossAsCells(t *testing.T) {
	var props map[string]notionPageProperty
	if err := json.Unmarshal([]byte(`{
		"Title":{"type":"title","title":[{"plain_text":"Launch & learn"}]},
		"Tags":{"type":"multi_select","multi_select":[{"name":"web"},{"name":"ios"}]},
		"Cost":{"type":"formula","formula":{"type":"number","number":42}},
		"Ref":{"type":"unique_id","unique_id":{"prefix":"REL","number":12}},
		"Made":{"type":"created_time","created_time":"2026-10-01T08:00:00.000Z"},
		"Owners":{"type":"people","people":[{"id":"u1","name":"Maya"},{"id":"u2","name":"Ada"}]},
		"Done":{"type":"checkbox","checkbox":false},
		"Notes":{"type":"rich_text","rich_text":[]}}`), &props); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"Title": "Launch & learn", "Cost": 42.0, "Ref": "REL-12", "Made": "2026-10-01", "Owners": "Maya, Ada", "Done": false, "Notes": ""}
	for name, v := range want {
		if got := cellOf(props[name]); got != v {
			t.Errorf("%s: %#v, want %#v", name, got, v)
		}
	}
	if tags, _ := cellOf(props["Tags"]).([]string); strings.Join(tags, ",") != "web,ios" {
		t.Errorf("tags %v", tags)
	}
}

// A link to a Notion page points at it in OneCamp when it came across: as a
// page of the tree, or a doc an earlier tree of the import made.
func TestLinksToNotionPagesPointAtTheirDocs(t *testing.T) {
	p, _, _ := withFakeNotion(t, nil)
	earlier := uuid.New()
	lookupMapped = func(_ context.Context, _ uuid.UUID, entity, source string) (uuid.UUID, error) {
		if entity == importModels.EntityDoc && source == "abababababababababababababababab" {
			return earlier, nil
		}
		return uuid.Nil, nil
	}
	tr := &tree{p: p, ctx: context.Background(), job: &importModels.Job{Id: uuid.New()}, titles: map[string]string{deploys: "Deploys"}}
	for href, want := range map[string]string{
		"/" + deploys: deploys,
		"https://www.notion.so/acme/Deploys-" + deploys + "?pvs=4": deploys,
		"https://www.notion.so/abababababababababababababababab":   "/app/doc/" + earlier.String(),
		"https://example.com/guide":                                "https://example.com/guide",
	} {
		h := href
		if got := tr.linkHref(notionRichText{Href: &h}); got != want {
			t.Errorf("%s: %q, want %q", href, got, want)
		}
	}
}

func ptrUUID(u uuid.UUID) *uuid.UUID { return &u }
