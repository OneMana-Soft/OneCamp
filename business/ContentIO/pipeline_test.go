package business

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/services/collab"
)

// A tiny PNG: an image the uploader is handed whole.
var png1x1 = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

func zipOf(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for name, data := range files {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write(data)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// docxOf is a minimal Word file: a document body, its styles, its lists, one
// image and one link.
func docxOf(t *testing.T, body string) []byte {
	t.Helper()
	const ns = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing"`
	return zipOf(t, map[string][]byte{
		"[Content_Types].xml": []byte(`<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`),
		"word/document.xml":   []byte(`<?xml version="1.0"?><w:document ` + ns + `><w:body>` + body + `</w:body></w:document>`),
		"word/styles.xml": []byte(`<?xml version="1.0"?><w:styles ` + ns + `>` +
			`<w:style w:styleId="Title"><w:name w:val="Title"/></w:style>` +
			`<w:style w:styleId="Heading1"><w:name w:val="heading 1"/></w:style>` +
			`<w:style w:styleId="Heading2"><w:name w:val="heading 2"/></w:style>` +
			`<w:style w:styleId="ListParagraph"><w:name w:val="List Paragraph"/></w:style></w:styles>`),
		"word/numbering.xml": []byte(`<?xml version="1.0"?><w:numbering ` + ns + `>` +
			`<w:abstractNum w:abstractNumId="0"><w:lvl w:ilvl="0"><w:numFmt w:val="bullet"/></w:lvl><w:lvl w:ilvl="1"><w:numFmt w:val="bullet"/></w:lvl></w:abstractNum>` +
			`<w:abstractNum w:abstractNumId="1"><w:lvl w:ilvl="0"><w:numFmt w:val="decimal"/></w:lvl></w:abstractNum>` +
			`<w:num w:numId="1"><w:abstractNumId w:val="0"/></w:num><w:num w:numId="2"><w:abstractNumId w:val="1"/></w:num></w:numbering>`),
		"word/_rels/document.xml.rels": []byte(`<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId5" Type="image" Target="media/image1.png"/>` +
			`<Relationship Id="rId6" Type="hyperlink" Target="https://onemana.dev/" TargetMode="External"/></Relationships>`),
		"word/media/image1.png": png1x1,
	})
}

const wordBody = `<w:p><w:pPr><w:pStyle w:val="Title"/></w:pPr><w:r><w:t>Quarterly report</w:t></w:r></w:p>` +
	`<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>Summary</w:t></w:r></w:p>` +
	`<w:p><w:r><w:t xml:space="preserve">We </w:t></w:r><w:r><w:rPr><w:b/></w:rPr><w:t>shipped</w:t></w:r><w:r><w:t xml:space="preserve"> it, see </w:t></w:r>` +
	`<w:hyperlink r:id="rId6"><w:r><w:t>the site</w:t></w:r></w:hyperlink><w:r><w:t>.</w:t></w:r></w:p>` +
	`<w:p><w:pPr><w:numPr><w:ilvl w:val="0"/><w:numId w:val="1"/></w:numPr></w:pPr><w:r><w:t>one</w:t></w:r></w:p>` +
	`<w:p><w:pPr><w:numPr><w:ilvl w:val="1"/><w:numId w:val="1"/></w:numPr></w:pPr><w:r><w:t>nested</w:t></w:r></w:p>` +
	`<w:p><w:pPr><w:numPr><w:ilvl w:val="0"/><w:numId w:val="2"/></w:numPr></w:pPr><w:r><w:t>first</w:t></w:r></w:p>` +
	`<w:tbl><w:tr><w:tc><w:p><w:r><w:t>Name</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>Done</w:t></w:r></w:p></w:tc></w:tr>` +
	`<w:tr><w:tc><w:p><w:r><w:t>Launch</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>yes</w:t></w:r></w:p></w:tc></w:tr></w:tbl>` +
	`<w:p><w:r><w:drawing><wp:inline><wp:docPr id="1" name="Picture" descr="A chart"/><a:graphic><a:graphicData><a:blip r:embed="rId5"/></a:graphicData></a:graphic></wp:inline></w:drawing></w:r></w:p>`

func TestAMarkdownFileBecomesADocTitledByItsHeading(t *testing.T) {
	w := newWorld(t)
	job := w.importFile("notes.md", []byte("# Launch plan\n\n- [ ] book the room\n- [x] write it\n\n| a | b |\n|---|---|\n| 1 | 2 |\n"), "")
	if job.Status != JobDone || len(job.Created) != 1 || job.Progress != 100 {
		t.Fatalf("got %+v", job)
	}
	d := w.docTitled("Launch plan")
	html := w.seeded[d.uuid]
	if !strings.Contains(html, `data-type="taskList"`) || strings.Contains(html, "<h1>") {
		t.Fatalf("seeded %s", html)
	}
	// The editor has simple tables (docs programme B3): the table is a table.
	if !strings.Contains(html, `<table><tbody><tr><th><p>a</p></th><th><p>b</p></th></tr><tr><td><p>1</p></td><td><p>2</p></td></tr></tbody></table>`) {
		t.Fatalf("the table was not kept: %s", html)
	}
	if len(w.events) != 1 || w.events[0].Kind != resourceEventBusiness.DocCreated {
		t.Fatalf("events %+v", w.events)
	}
	if len(w.files) != 0 {
		t.Fatalf("the upload should be gone once the job is done: %v", w.files)
	}
}

// A Markdown file whose quotes or lists nest past what the reader takes
// (contentcodec.ErrTooDeep) still comes in, as plain text with every word,
// titled by its name: it neither stops the import nor reaches the parser,
// which took six minutes over half a megabyte of nested quotes.
func TestAMarkdownFileNestedTooDeepComesInAsPlainText(t *testing.T) {
	w := newWorld(t)
	md := strings.Repeat(">", helpers.MaxMarkdownNesting+1) + " the deepest note\n"
	job := w.importFile("deep.md", []byte(md), "")
	if job.Status != JobDone || len(job.Created) != 1 {
		t.Fatalf("got %+v", job)
	}
	html := w.seeded[w.docTitled("deep").uuid]
	if strings.Contains(html, "<blockquote>") || !strings.Contains(html, "the deepest note") {
		t.Fatalf("seeded %s", html)
	}
}

func TestMarkdownImportExportImportKeepsTheBlocks(t *testing.T) {
	md := "# Plan\n\n## Steps\n\n1. one\n2. two\n   - nested\n\n- [ ] task\n- [x] done\n\n| Step | Owner |\n| --- | --- |\n| Draft | **Sam** |\n\n> [!NOTE]\n> A callout.\n"
	w := newWorld(t)
	w.importFile("plan.md", []byte(md), "")
	d := w.docTitled("Plan")
	body, err := contentcodec.FromHTML(w.seeded[d.uuid], contentcodec.HTMLOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := docMarkdown(d.title, body, ""); got != md {
		t.Fatalf("export after import differs:\n%s\nwant\n%s", got, md)
	}
}

func TestAZipOfThreeFilesMakesThreeDocsWithTheirImages(t *testing.T) {
	w := newWorld(t)
	data := zipOf(t, map[string][]byte{
		"Wiki/Home.md":            []byte("# Home\n\nSee [Setup](Setup.md) and ![logo](img/logo.png).\n"),
		"Wiki/Setup.html":         []byte("<html><head><title>Setup</title></head><body><p>Install <b>it</b>.</p><img src=\"img/logo.png\" alt=\"logo\"></body></html>"),
		"Wiki/Report.docx":        docxOf(t, wordBody),
		"Wiki/img/logo.png":       png1x1,
		"__MACOSX/Wiki/._Home.md": []byte("junk"),
	})
	job := w.importFile("wiki.zip", data, "")
	if job.Status != JobDone || len(job.Created) != 3 {
		t.Fatalf("got %+v", job)
	}
	home, setup, report := w.docTitled("Home"), w.docTitled("Setup"), w.docTitled("Quarterly report")
	if !strings.Contains(w.seeded[home.uuid], `href="/app/doc/`+setup.uuid+`"`) {
		t.Fatalf("the link to Setup was not pointed at its doc: %s", w.seeded[home.uuid])
	}
	for _, d := range []fakeDoc{home, setup, report} {
		html := w.seeded[d.uuid]
		if !strings.Contains(html, "https://api.example.test/doc/getDocAttachment/"+d.uuid+"/") {
			t.Fatalf("%s: its image is not in storage: %s", d.title, html)
		}
		if strings.Contains(html, "base64") || strings.Contains(html, "blob:") {
			t.Fatalf("%s: %s", d.title, html)
		}
	}
	if len(w.assets) != 3 {
		t.Fatalf("uploaded %v", w.assets)
	}
}

// An import's pages are seeded quietly: what they say was written somewhere
// else, so the docs keep whom they mention and tell nobody (business/Doc
// whoToTell). Every page of the import is.
func TestAnImportsPagesAreSeededQuietly(t *testing.T) {
	w := newWorld(t)
	data := zipOf(t, map[string][]byte{
		"Wiki/Home.md":  []byte("# Home\n\nSee [Setup](Setup.md).\n"),
		"Wiki/Setup.md": []byte("# Setup\n\nInstall it.\n"),
	})
	job := w.importFile("wiki.zip", data, "")
	if job.Status != JobDone || len(job.Created) != 2 || len(w.seeded) != 2 {
		t.Fatalf("got %+v, seeded %d", job, len(w.seeded))
	}
	for _, title := range []string{"Home", "Setup"} {
		if d := w.docTitled(title); !w.quiet[d.uuid] {
			t.Errorf("%s was seeded loudly: an import tells nobody whom it mentions", title)
		}
	}
}

func TestAWordFileKeepsHeadingsListsLinksAndTables(t *testing.T) {
	w := newWorld(t)
	w.importFile("report.docx", docxOf(t, wordBody), "")
	d := w.docTitled("Quarterly report")
	body, _ := contentcodec.FromHTML(w.seeded[d.uuid], contentcodec.HTMLOptions{})
	md := contentcodec.ToMarkdown(body, contentcodec.MarkdownOptions{})
	for _, want := range []string{
		"# Summary",
		"We **shipped** it, see [the site](https://onemana.dev/).",
		"- one\n  - nested",
		"1. first",
		"| Name | Done |",
		"![A chart](https://api.example.test/doc/getDocAttachment/",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
}

func TestPagesInAFolderGoUnderItsPage(t *testing.T) {
	w := newWorld(t)
	data := zipOf(t, map[string][]byte{
		"Plans.md":        []byte("Top"),
		"Plans/Q4.md":     []byte("Fourth quarter"),
		"Loose/Notes.txt": []byte("plain"),
	})
	job := w.importFile("plans.zip", data, "")
	if job.Status != JobDone {
		t.Fatalf("got %+v", job)
	}
	plans, q4, loose, notes := w.docTitled("Plans"), w.docTitled("Q4"), w.docTitled("Loose"), w.docTitled("Notes")
	if q4.parent != plans.uuid || notes.parent != loose.uuid || plans.parent != "" {
		t.Fatalf("docs %+v", w.docs)
	}
}

func TestAnImportGoesOnlyUnderAPageTheyCanEdit(t *testing.T) {
	w := newWorld(t)
	parent, readOnly := "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	w.levels[parent] = accessBusiness.LevelEdit
	w.levels[readOnly] = accessBusiness.LevelView
	job := w.importFile("a.md", []byte("x"), parent)
	if job.Status != JobDone || w.docs[0].parent != parent {
		t.Fatalf("got %+v %+v", job, w.docs)
	}
	_, err := Pipeline.Import(context.Background(), tester, ImportRequest{Filename: "b.md", ParentUUID: readOnly, Body: strings.NewReader("x")})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("got %v", err)
	}
	// An upload of its own (POST /doc/import/presign) is refused the same.
	if _, _, err := Pipeline.PrepareUpload(context.Background(), tester, ImportRequest{Filename: "b.md", ParentUUID: readOnly, Size: 1}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an upload under a page they may only read: %v", err)
	}
	_, err = Pipeline.Import(context.Background(), tester, ImportRequest{Filename: "b.pdf", Body: strings.NewReader("x")})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("got %v", err)
	}
}

func TestADocTheServiceCannotFillIsNotLeftBehind(t *testing.T) {
	w := newWorld(t)
	w.seedErr = errors.New("collab down")
	job := w.importFile("a.md", []byte("hello"), "")
	if job.Status != JobFailed || len(w.deleted) != 1 || len(job.Created) != 0 {
		t.Fatalf("got %+v, deleted %v", job, w.deleted)
	}
	if job.Error != "No doc could be made: "+whyNotFilled+"." {
		t.Fatalf("said %q", job.Error)
	}
}

// With the document service not running, the first seed says so, and no
// other is asked for.
func TestWithTheServiceDownNoMoreSeedsAreTried(t *testing.T) {
	w := newWorld(t)
	w.seedErr = collab.ErrUnavailable
	data := zipOf(t, map[string][]byte{"A.md": []byte("a"), "B.md": []byte("b"), "C.md": []byte("c")})
	job := w.importFile("three.zip", data, "")
	if job.Status != JobFailed || w.seeds != 1 || len(w.deleted) != 3 || len(job.Created) != 0 {
		t.Fatalf("got %+v; %d seeds, deleted %v", job, w.seeds, w.deleted)
	}
}

// A page whose own content fails stays when pages under it were imported:
// removing it would take them too.
func TestAPageThatFailsKeepsThePagesUnderIt(t *testing.T) {
	w := newWorld(t)
	w.failTitles = map[string]bool{"Plans": true, "Loose": true}
	data := zipOf(t, map[string][]byte{
		"Plans.md":    []byte("Top"),
		"Plans/Q4.md": []byte("Fourth quarter"),
		"Loose.md":    []byte("on its own"),
	})
	job := w.importFile("plans.zip", data, "")
	if job.Status != JobDone {
		t.Fatalf("got %+v", job)
	}
	plans, q4, loose := w.docTitled("Plans"), w.docTitled("Q4"), w.docTitled("Loose")
	if q4.parent != plans.uuid || len(w.deleted) != 1 || w.deleted[0] != loose.uuid {
		t.Fatalf("docs %+v, deleted %v", w.docs, w.deleted)
	}
	var made []string
	for _, c := range job.Created {
		made = append(made, c.Title)
	}
	if strings.Join(made, ",") != "Plans,Q4" {
		t.Fatalf("created %v", made)
	}
	why := map[string]string{}
	for _, s := range job.Skipped {
		why[s.What] = s.Why
	}
	if why["Plans.md"] != whyHolder || why["Loose.md"] != whyNotFilled {
		t.Fatalf("skipped %+v", job.Skipped)
	}
}

// An empty doc that can't be taken away again is listed, and the job says
// so, never leaving it behind silently.
func TestAnEmptyDocThatCannotBeRemovedIsSaid(t *testing.T) {
	w := newWorld(t)
	w.seedErr = errors.New("collab down")
	w.discardErr = ErrUnavailable
	job := w.importFile("a.md", []byte("# Notes\n\nhello"), "")
	if job.Status != JobFailed || len(job.Created) != 1 || job.Created[0].Title != "Notes" {
		t.Fatalf("got %+v", job)
	}
	if !strings.Contains(job.Error, "The empty doc it made could not be removed") {
		t.Fatalf("said %q", job.Error)
	}
	if len(job.Skipped) != 1 || job.Skipped[0].Why != whyLeftEmpty {
		t.Fatalf("skipped %+v", job.Skipped)
	}
}

func TestSomeoneElsesJobIsNotFound(t *testing.T) {
	w := newWorld(t)
	job := w.importFile("a.md", []byte("x"), "")
	other := accessBusiness.Subject{UserUID: "0x9", UserUUID: "99999999-9999-9999-9999-999999999999"}
	if _, err := Pipeline.Job(context.Background(), other, job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestALargeFileGoesThroughStorage(t *testing.T) {
	w := newWorld(t)
	job, up, err := Pipeline.PrepareUpload(context.Background(), tester, ImportRequest{Filename: "big.md", Size: 20 << 20})
	if err != nil || job.Status != JobUploading || up.Method != "PUT" || !strings.Contains(up.URL, job.ID) {
		t.Fatalf("got %+v %+v %v", job, up, err)
	}
	// Started before the file arrived: not found.
	if _, err := Pipeline.StartUploaded(context.Background(), tester, job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	for key := range w.jobs {
		w.files[*w.jobs[key].InputKey] = []byte("# Big\n\ntext")
	}
	started, err := Pipeline.StartUploaded(context.Background(), tester, job.ID)
	if err != nil || started.Status != JobQueued {
		t.Fatalf("got %+v %v", started, err)
	}
	if err := runQueued(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.docTitled("Big").uuid == "" {
		t.Fatal("no doc")
	}
}

type bufTarget struct {
	name, mediaType string
	bytes.Buffer
}

func (b *bufTarget) Start(name, mediaType string) io.Writer {
	b.name, b.mediaType = name, mediaType
	return &b.Buffer
}

func TestTheServerExportsMarkdownHTMLAndJSON(t *testing.T) {
	w := newWorld(t)
	id := "33333333-3333-3333-3333-333333333333"
	w.levels[id] = accessBusiness.LevelView
	w.bodies[id] = [2]string{"Launch notes", `<p xmlns="http://www.w3.org/1999/xhtml">Hello <strong>world</strong></p><ul data-type="taskList"><li data-type="taskItem" data-checked="true"><p>done</p></li></ul>`}
	res := accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: id}
	for format, want := range map[string]string{
		"md":   "# Launch notes\n\nHello **world**\n\n- [x] done\n",
		"html": "<h1 class=\"doc-title\">Launch notes</h1>\n<p>Hello <strong>world</strong></p>",
		"json": `{"title":"Launch notes","doc":{`,
	} {
		var out bufTarget
		if err := Pipeline.Export(context.Background(), tester, res, format, &out); err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if !strings.Contains(out.String(), want) || !strings.HasPrefix(out.name, "Launch notes.") {
			t.Fatalf("%s: %s %q", format, out.name, out.String())
		}
		if format == "json" {
			var v map[string]any
			if err := json.Unmarshal(out.Bytes(), &v); err != nil {
				t.Fatalf("json: %v", err)
			}
		}
	}
	if len(w.events) != 3 || w.events[0].Kind != resourceEventBusiness.DocExported || w.events[0].Data["via"] != "server" {
		t.Fatalf("events %+v", w.events)
	}
	// PDF and Word are made in the browser; the server records them.
	if err := Pipeline.Export(context.Background(), tester, res, "pdf", &bufTarget{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("got %v", err)
	}
	if err := Pipeline.RecordExport(context.Background(), tester, res, "docx"); err != nil || w.events[3].Data["via"] != "browser" {
		t.Fatalf("got %v %+v", err, w.events)
	}
	// Nobody exports what they can't read.
	other := "44444444-4444-4444-4444-444444444444"
	w.levels[other] = accessBusiness.LevelNone
	if err := Pipeline.RecordExport(context.Background(), tester, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: other}, "pdf"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

// An importer of something other than docs (calendar A4) is chosen only for
// an import into what it makes, makes it itself, and the job reports it as
// it reports docs: a zip into docs still goes to the zip of docs.
type fakeTargeted struct {
	got  []string
	made Made
}

func (f *fakeTargeted) Format() string                              { return "fake-calendar" }
func (f *fakeTargeted) Accepts(filename, _ string) bool             { return hasExt(filename, ".ics", ".zip") }
func (f *fakeTargeted) Into() string                                { return IntoCalendar }
func (f *fakeTargeted) Import(context.Context, Input, Output) error { return nil }
func (f *fakeTargeted) Make(_ context.Context, who accessBusiness.Subject, in Input, _ json.RawMessage, report func(done, total int)) (Made, error) {
	data, _ := io.ReadAll(in.Reader())
	f.got = append(f.got, who.UserUUID+" "+in.Filename+" "+string(data))
	report(1, 2)
	return f.made, nil
}

func TestAnImportIntoACalendarIsMadeByItsOwnImporter(t *testing.T) {
	w := newWorld(t)
	// A calendar's import names the first of what it made, says how many and
	// where they went, and what to show first (calendar A4's review).
	standup := Created{Kind: "event", UUID: "e1", Title: "Standup", At: "2026-10-22T09:00:00Z"}
	imp := &fakeTargeted{made: Made{Created: []Created{standup}, Count: 130, Into: []Into{{UUID: "c1", Title: "Family", Count: 130}}, Show: &standup,
		Skipped: []Skipped{{What: "A to-do", Why: "Only events are read."}}}}
	thePipeline.RegisterImporter(imp)
	t.Cleanup(func() {
		thePipeline.mu.Lock()
		defer thePipeline.mu.Unlock()
		thePipeline.importers = thePipeline.importers[:len(thePipeline.importers)-1]
	})
	job, err := Pipeline.Import(context.Background(), tester, ImportRequest{Filename: "jonas.ics", Into: IntoCalendar, Size: 5, Body: strings.NewReader("BEGIN")})
	if err != nil {
		t.Fatal(err)
	}
	if err := runQueued(context.Background()); err != nil {
		t.Fatal(err)
	}
	done, err := Pipeline.Job(context.Background(), tester, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != JobDone || done.Format != "fake-calendar" || len(done.Created) != 1 || done.Created[0].Title != "Standup" || len(done.Skipped) != 1 {
		t.Fatalf("job %+v", done)
	}
	if done.Made != 130 || len(done.Into) != 1 || done.Into[0] != (Into{UUID: "c1", Title: "Family", Count: 130}) || done.Show == nil || *done.Show != standup {
		t.Fatalf("how many, where, and what to show: %d %+v %+v", done.Made, done.Into, done.Show)
	}
	if len(imp.got) != 1 || imp.got[0] != tester.UserUUID+" jonas.ics BEGIN" {
		t.Fatalf("made from %v", imp.got)
	}
	if len(w.docs) != 0 {
		t.Fatalf("an import into a calendar made docs: %+v", w.docs)
	}
	// Nothing goes under a page from an import into a calendar, and a file
	// into docs never reaches the calendar's importer.
	if _, err := Pipeline.Import(context.Background(), tester, ImportRequest{Filename: "jonas.ics", Into: IntoCalendar, ParentUUID: "doc-1", Size: 5, Body: strings.NewReader("BEGIN")}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("under a page: %v", err)
	}
	if _, err := Pipeline.Import(context.Background(), tester, ImportRequest{Filename: "jonas.ics", Size: 5, Body: strings.NewReader("BEGIN")}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("an .ics into docs: %v", err)
	}
	if len(imp.got) != 1 {
		t.Fatalf("the calendar's importer ran for docs: %v", imp.got)
	}
}
