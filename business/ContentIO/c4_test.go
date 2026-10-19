package business

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	contentIOModels "github.com/akashc777/OneCamp/models/postgres/ContentIO"
	"github.com/google/uuid"
)

// A workspace's docs, as the export reads them: rows, who owns each, and the
// consents their owners gave.
type workspace struct {
	rows     []*dgraphStruct.DgraphDoc
	consents map[string]string
	admin    bool
}

func (w *world) useWorkspace(ws *workspace) {
	saved := struct {
		page     func(context.Context, int, int) ([]string, error)
		rows     func(context.Context, []string, string) ([]*dgraphStruct.DgraphDoc, error)
		consents func(context.Context, []string) (map[string]string, error)
		admin    func(context.Context, string) bool
	}{docUUIDPage, docRowsOf, consentsFor, isAdmin}
	w.t.Cleanup(func() {
		docUUIDPage, docRowsOf, consentsFor, isAdmin = saved.page, saved.rows, saved.consents, saved.admin
	})
	docUUIDPage = func(_ context.Context, offset, first int) ([]string, error) {
		var ids []string
		for i := offset; i < len(ws.rows) && i < offset+first; i++ {
			ids = append(ids, ws.rows[i].Uuid)
		}
		return ids, nil
	}
	docRowsOf = func(_ context.Context, ids []string, _ string) ([]*dgraphStruct.DgraphDoc, error) {
		var out []*dgraphStruct.DgraphDoc
		for _, id := range ids {
			for _, r := range ws.rows {
				if r.Uuid == id {
					out = append(out, r)
				}
			}
		}
		return out, nil
	}
	consentsFor = func(context.Context, []string) (map[string]string, error) { return ws.consents, nil }
	isAdmin = func(context.Context, string) bool { return ws.admin }
}

func row(id, title, parent, owner string, private bool) *dgraphStruct.DgraphDoc {
	d := &dgraphStruct.DgraphDoc{Uuid: id, Title: title, IsPrivate: &private, Owner: &dgraphStruct.DgraphUser{Uuid: owner}}
	if parent != "" {
		d.Parent = &dgraphStruct.DgraphDoc{Uuid: parent}
	}
	return d
}

func zipNames(t *testing.T, data []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = string(b)
	}
	return out
}

// ai-search-io.md P2-4: an admin's export holds every doc they can open, and
// a private doc they can't only when its owner let it in, and says how many
// it left out. Subpages sit in their page's folder.
func TestAWorkspaceExportLeavesOutPrivateDocsUnlessTheirOwnerLetThemIn(t *testing.T) {
	w := newWorld(t)
	maya := uuid.NewString()
	handbook, onboarding, diary, plans, gone, shelved := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	archived := row(shelved, "Shelved", "", maya, false)
	archivedAt := time.Now().Add(-time.Hour)
	archived.ArchivedAt = &archivedAt
	// A table row's page (a hosted doc, D-18) goes with its table: never in
	// the docs' export, nor counted among the private docs left out, even
	// where something decided it open.
	acmePage := uuid.NewString()
	hosted := row(acmePage, "Acme", "", "", true)
	hosted.Owner, hosted.HostKind, hosted.HostUUID = nil, "table_row", uuid.NewString()
	w.useWorkspace(&workspace{admin: true, rows: []*dgraphStruct.DgraphDoc{
		row(handbook, "Handbook", "", maya, false),
		row(onboarding, "Onboarding", handbook, maya, false),
		row(diary, "Diary", "", maya, true),
		row(plans, "Plans", "", maya, true),
		row(gone, "Gone", "", maya, true),
		archived,
		hosted,
	}, consents: map[string]string{plans: maya, gone: uuid.NewString()}})
	w.levels[handbook], w.levels[onboarding], w.levels[acmePage] = accessBusiness.LevelView, accessBusiness.LevelEdit, accessBusiness.LevelEdit
	w.bodies[acmePage] = [2]string{"Acme", "<p>Call on Friday.</p>"}
	for id, title := range map[string]string{handbook: "Handbook", onboarding: "Onboarding", diary: "Diary", plans: "Plans", gone: "Gone"} {
		w.bodies[id] = [2]string{title, "<p>About " + title + "</p>"}
	}

	job, err := ExportWorkspace(context.Background(), tester)
	if err != nil {
		t.Fatal(err)
	}
	if err := runQueued(context.Background()); err != nil {
		t.Fatal(err)
	}
	done, url, err := WorkspaceExport(context.Background(), tester, job.ID)
	if err != nil || done.Status != JobDone || done.Exported != 3 {
		t.Fatalf("job %+v, %v", done, err)
	}
	if !strings.Contains(url, "workspace-docs.zip") {
		t.Fatalf("download address %q", url)
	}
	var key string
	for k := range w.files {
		if strings.HasSuffix(k, "workspace-docs.zip") {
			key = k
		}
	}
	files := zipNames(t, w.files[key])
	var names []string
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "Handbook.md,Handbook/Onboarding.md,Plans.md" {
		t.Fatalf("the zip holds %v", names)
	}
	if files["Plans.md"] != "# Plans\n\nAbout Plans\n" {
		t.Fatalf("Plans.md is %q", files["Plans.md"])
	}
	if len(done.Skipped) != 1 || done.Skipped[0].What != "2 private docs" {
		t.Fatalf("skipped %+v", done.Skipped)
	}
}

// A second export while the first still runs is the first, so one click
// twice is one export.
func TestASecondWorkspaceExportIsTheOneStillRunning(t *testing.T) {
	w := newWorld(t)
	w.useWorkspace(&workspace{admin: true})
	first, err := ExportWorkspace(context.Background(), tester)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ExportWorkspace(context.Background(), tester)
	if !errors.Is(err, ErrConflict) || again.ID != first.ID {
		t.Fatalf("a second export while the first waits: %+v, %v", again, err)
	}
	if err := runQueued(context.Background()); err != nil {
		t.Fatal(err)
	}
	if next, err := ExportWorkspace(context.Background(), tester); err != nil || next.ID == first.ID {
		t.Fatalf("an export once the first is done: %+v, %v", next, err)
	}
	recent, err := RecentWorkspaceExports(context.Background(), tester)
	if err != nil || len(recent) != 2 || recent[1].ID != first.ID {
		t.Fatalf("recent exports %+v, %v", recent, err)
	}
}

func TestOnlyAnAdminExportsTheWorkspace(t *testing.T) {
	w := newWorld(t)
	w.useWorkspace(&workspace{})
	if _, err := ExportWorkspace(context.Background(), tester); !errors.Is(err, ErrNotAdmin) {
		t.Fatalf("a member's export: %v", err)
	}
}

// Someone else's workspace export is not found.
func TestSomeoneElsesWorkspaceExportIsNotFound(t *testing.T) {
	w := newWorld(t)
	w.useWorkspace(&workspace{admin: true})
	job, err := ExportWorkspace(context.Background(), tester)
	if err != nil {
		t.Fatal(err)
	}
	other := accessBusiness.Subject{UserUID: "0x9", UserUUID: uuid.NewString()}
	if _, _, err := WorkspaceExport(context.Background(), other, job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another admin read it: %v", err)
	}
}

func TestOnlyADocsOwnerLetsItIntoExports(t *testing.T) {
	w := newWorld(t)
	mine, theirs := uuid.NewString(), uuid.NewString()
	w.levels[mine], w.levels[theirs] = accessBusiness.LevelFull, accessBusiness.LevelFull
	w.owned[mine] = true
	w.useWorkspace(&workspace{rows: []*dgraphStruct.DgraphDoc{
		row(mine, "Mine", "", tester.UserUUID, true), row(theirs, "Theirs", "", uuid.NewString(), true),
	}})
	var allowed []uuid.UUID
	savedAllow, savedWithdraw := allowExport, withdrawExport
	t.Cleanup(func() { allowExport, withdrawExport = savedAllow, savedWithdraw })
	allowExport = func(_ context.Context, doc, _ uuid.UUID) error { allowed = append(allowed, doc); return nil }
	withdrawExport = func(context.Context, uuid.UUID) error { return nil }

	if err := SetExportConsent(context.Background(), tester, mine, true); err != nil {
		t.Fatal(err)
	}
	if state, err := ExportConsent(context.Background(), tester, theirs); err != nil || state.Owner || !state.Private {
		t.Fatalf("someone else's private doc reads as theirs to change: %+v, %v", state, err)
	}
	if err := SetExportConsent(context.Background(), tester, theirs, true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("someone with full access but no ownership: %v", err)
	}
	if len(allowed) != 1 || allowed[0].String() != mine {
		t.Fatalf("allowed %v", allowed)
	}
}

func TestExportPathsFollowTheTreeAndTellSiblingsApart(t *testing.T) {
	got := exportPaths([]exportDoc{
		{uuid: "a", title: "Notes"}, {uuid: "b", title: "Notes"}, {uuid: "c", title: "", parent: "a"},
		{uuid: "d", title: "Loop", parent: "e"}, {uuid: "e", title: "Back", parent: "d"},
	})
	want := map[string]string{"a": "Notes", "b": "Notes (2)", "c": "Notes/Untitled"}
	for id, p := range want {
		if got[id] != p {
			t.Errorf("%s: %q, want %q", id, got[id], p)
		}
	}
	if got["d"] == "" || got["e"] == "" {
		t.Fatalf("a loop lost its docs: %v", got)
	}
}

// MakePages makes a tree of pages, with their icons, under a page who can
// edit, and refuses one they can't.
func TestMakePagesMakesATreeUnderAPageTheyCanEdit(t *testing.T) {
	w := newWorld(t)
	parent := uuid.NewString()
	w.levels[parent] = accessBusiness.LevelEdit
	body := func(s string) *contentcodec.Node { n, _ := contentcodec.FromMarkdown(s, nil); return n }
	made, err := MakePages(context.Background(), tester, parent, []Page{
		{Key: "a", Title: "Roadmap", Icon: "🗺️", Body: body("Plans")},
		{Key: "b", ParentKey: "a", Title: "Q4", Body: body("Fourth")},
	})
	if err != nil || made.Filled != 2 || len(made.Created) != 2 {
		t.Fatalf("made %+v, %v", made, err)
	}
	roadmap, q4 := w.docTitled("Roadmap"), w.docTitled("Q4")
	if roadmap.parent != parent || q4.parent != roadmap.uuid || roadmap.icon != "🗺️" {
		t.Fatalf("docs %+v", w.docs)
	}
	w.levels[parent] = accessBusiness.LevelView
	if _, err := MakePages(context.Background(), tester, parent, []Page{{Key: "c", Title: "No", Body: body("x")}}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("under a page they can only read: %v", err)
	}
}

// An imported page keeps its icon only when it is short enough to be one
// emoji: a page's header holding a run of text gives its doc no icon.
func TestAnImportedPageKeepsOnlyAnIconThatIsOne(t *testing.T) {
	w := newWorld(t)
	body := func(s string) *contentcodec.Node { n, _ := contentcodec.FromMarkdown(s, nil); return n }
	if _, err := MakePages(context.Background(), tester, "", []Page{
		{Key: "a", Title: "Family", Icon: "👨‍👩‍👧‍👦", Body: body("Plans")},
		{Key: "b", Title: "Header", Icon: strings.Repeat("Notes ", 20), Body: body("Text")},
	}); err != nil {
		t.Fatal(err)
	}
	if family, header := w.docTitled("Family"), w.docTitled("Header"); family.icon != "👨‍👩‍👧‍👦" || header.icon != "" {
		t.Fatalf("icons %q and %q", family.icon, header.icon)
	}
}

// Notion splits a large export into zips inside the zip it gives: their pages
// import as one tree, and what reading them left on disk is removed.
func TestTheZipsInsideAZipImportAsOne(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	w := newWorld(t)
	part1 := zipOf(t, map[string][]byte{"Plans.md": []byte("# Plans\n\nQ4."), "Plans/Launch.md": []byte("# Launch\n\nSoon.")})
	part2 := zipOf(t, map[string][]byte{"Notes.md": []byte("# Notes\n\nHello.")})
	job := w.importFile("Export-1a2b.zip", zipOf(t, map[string][]byte{"Export-1a2b-Part-1.zip": part1, "Export-1a2b-Part-2.zip": part2}), "")
	if job.Status != JobDone {
		t.Fatalf("job %+v", job)
	}
	plans, launch := w.docTitled("Plans"), w.docTitled("Launch")
	w.docTitled("Notes")
	if launch.parent != plans.uuid || len(w.docs) != 3 {
		t.Fatalf("docs %+v", w.docs)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Fatalf("left on disk: %v", left)
	}
}

// A file is read in whatever it was saved in: UTF-8 with or without its
// mark, UTF-16 as Windows' Notepad saves "Unicode", the character set an HTML
// page declares, and Windows-1252 for the rest.
func TestImportedTextIsReadInWhateverItWasSavedIn(t *testing.T) {
	utf16 := func(s string, big bool) []byte {
		out := []byte{0xff, 0xfe}
		if big {
			out = []byte{0xfe, 0xff}
		}
		for _, r := range s {
			if big {
				out = append(out, byte(r>>8), byte(r))
			} else {
				out = append(out, byte(r), byte(r>>8))
			}
		}
		return out
	}
	for _, c := range []struct {
		name string
		data []byte
		want string
	}{
		{"utf-8", []byte("Café ☕ plans"), "Café ☕ plans"},
		{"utf-8 with its mark", []byte("\xef\xbb\xbfCafé plans"), "Café plans"},
		{"utf-16, little end first", utf16("Café plans", false), "Café plans"},
		{"utf-16, big end first", utf16("Café plans", true), "Café plans"},
		{"windows-1252", []byte("It\x92s \x93quoted\x94, \x80 5, caf\xe9"), "It’s “quoted”, € 5, café"},
		{"a declared charset", []byte("<meta charset=\"windows-1251\"><p>\xcf\xf0\xe8\xe2\xe5\xf2</p>"), `<meta charset="windows-1251"><p>Привет</p>`},
	} {
		if got := DecodeText(c.data); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// A Markdown file saved in Windows-1252 imports with its own quotes.
func TestAWindows1252FileImportsWithItsQuotes(t *testing.T) {
	w := newWorld(t)
	job := w.importFile("notes.md", []byte("# Q4\x92s plan\n\nShip it \x96 \x93soon\x94."), "")
	if job.Status != JobDone {
		t.Fatalf("job %+v", job)
	}
	d := w.docTitled("Q4’s plan")
	if !strings.Contains(w.seeded[d.uuid], "Ship it – “soon”.") {
		t.Fatalf("seeded %q", w.seeded[d.uuid])
	}
}

// A zip from Windows' own zip names its files in CP437, not UTF-8: a doc
// named after its file keeps its accents.
func TestAZipsCodePageNamesAreReadAsText(t *testing.T) {
	w := newWorld(t)
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	f, err := zw.CreateHeader(&zip.FileHeader{Name: "Caf\x82 notes.txt", NonUTF8: true, Method: zip.Deflate})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("Opening hours."))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if job := w.importFile("notes.zip", b.Bytes(), ""); job.Status != JobDone {
		t.Fatalf("job %+v", job)
	}
	w.docTitled("Café notes")
}

// A picture larger than an upload may be is turned away by its size, without
// being read: a wiki's export can hold a file of hundreds of megabytes.
func TestATooLargePictureIsNotRead(t *testing.T) {
	w := newWorld(t)
	data := zipOf(t, map[string][]byte{"Plan.md": []byte("# Plan\n\n![Map](map.png)"), "map.png": make([]byte, 24<<20)})
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	job := w.importFile("plan.zip", data, "")
	runtime.ReadMemStats(&after)
	if job.Status != JobDone || len(job.Skipped) != 1 || !strings.HasPrefix(job.Skipped[0].Why, "the image is 24 MB") {
		t.Fatalf("job %+v", job)
	}
	if read := after.TotalAlloc - before.TotalAlloc; read > 8<<20 {
		t.Fatalf("the import took %d MB of memory for a picture it turned away", read>>20)
	}
}

// A wiki's export holds more pictures than pages: a zip may hold up to 50,000
// files, of which up to 5,000 pages, each a doc.
func TestAZipMayHoldManyFilesButNoMoreThanFiveThousandPages(t *testing.T) {
	w := newWorld(t)
	files := map[string][]byte{"Plan.md": []byte("# Plan\n\nSoon.")}
	for i := 0; i < 6000; i++ {
		files[fmt.Sprintf("pictures/%d.png", i)] = []byte("png")
	}
	if job := w.importFile("wiki.zip", zipOf(t, files), ""); job.Status != JobDone || len(w.docs) != 1 {
		t.Fatalf("job %+v, docs %d", job, len(w.docs))
	}
	pages := map[string][]byte{}
	for i := 0; i <= maxZipPages; i++ {
		pages[fmt.Sprintf("page %d.md", i)] = []byte("x")
	}
	if job := w.importFile("pages.zip", zipOf(t, pages), ""); job.Status != JobFailed || !strings.Contains(job.Error, "an import makes at most 5000 docs") {
		t.Fatalf("job %+v", job)
	}
}

// With the document service going down part way, MakePages says to try
// again, and UnmakePages takes away what it made, so the pages can be made
// again whole; a page whose content is refused is no reason to.
func TestPagesTheServiceStoppedTakingCanBeMadeAgainWhole(t *testing.T) {
	w := newWorld(t)
	w.downAfter = 1
	body := func(s string) *contentcodec.Node { n, _ := contentcodec.FromMarkdown(s, nil); return n }
	pages := []Page{{Key: "a", Title: "Roadmap", Body: body("Plans")}, {Key: "b", ParentKey: "a", Title: "Q4", Body: body("Fourth")}, {Key: "c", Title: "Notes", Body: body("Later")}}
	made, err := MakePages(context.Background(), tester, "", pages)
	if err != nil || !made.TryAgain || len(made.Created) != 1 || made.Created[0].Title != "Roadmap" {
		t.Fatalf("made %+v, %v", made, err)
	}
	if left := UnmakePages(context.Background(), tester, made); left != 0 || !slices.Contains(w.deleted, made.Created[0].UUID) {
		t.Fatalf("%d left, deleted %v", left, w.deleted)
	}

	w = newWorld(t)
	w.failTitles = map[string]bool{"Notes": true}
	if made, err := MakePages(context.Background(), tester, "", pages); err != nil || made.TryAgain || len(made.Created) != 2 {
		t.Fatalf("a refused page: %+v, %v", made, err)
	}
}

// An export's job says until when its zip is kept, from the cleanup's own
// rule, and nothing once the zip is gone.
func TestAnExportSaysUntilWhenItsZipIsKept(t *testing.T) {
	done := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	key := "content-io/x/workspace-docs.zip"
	row := &contentIOModels.Job{ID: uuid.New(), Kind: "export", Status: contentIOModels.StatusDone, FinishedAt: &done, OutputKey: &key}
	if kept := toJob(row).KeptUntil; kept == nil || !kept.Equal(done.Add(keepFilesFor)) {
		t.Fatalf("kept until %v", kept)
	}
	row.OutputKey = nil
	if kept := toJob(row).KeptUntil; kept != nil {
		t.Fatalf("a removed zip is kept until %v", kept)
	}
}

// fullDisk is a file that takes nothing more.
type fullDisk struct{}

func (fullDisk) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }

// A zip that can't be written is no export: the error comes back, where it
// was logged and the zip stored as made.
func TestAnExportWhoseZipCantBeWrittenFails(t *testing.T) {
	w := newWorld(t)
	w.bodies["d1"] = [2]string{"Plan", "<p>Ship it.</p>"}
	if _, _, err := writeWorkspaceZip(context.Background(), fullDisk{}, tester, []exportDoc{{uuid: "d1", title: "Plan"}}, func(int) {}); err == nil {
		t.Fatal("a zip the disk refused came back as written")
	}
}

// A zip a layout claims is read by it; any other by folders.
func TestAZipLayoutReadsTheExportItClaims(t *testing.T) {
	w := newWorld(t)
	RegisterZipLayout(testLayout{})
	job := w.importFile("tool.zip", zipOf(t, map[string][]byte{"tool-export.marker": nil, "Page.md": []byte("hi")}), "")
	if job.Status != JobDone || w.docTitled("From the layout").uuid == "" {
		t.Fatalf("job %+v docs %+v", job, w.docs)
	}
}

type testLayout struct{}

func (testLayout) Name() string { return "test" }

func (testLayout) Claims(names []string) bool {
	for _, n := range names {
		if n == "tool-export.marker" {
			return true
		}
	}
	return false
}

func (testLayout) Read(_ context.Context, _ *ZipArchive, out Output) error {
	return out.Page(Page{Key: "x", Title: "From the layout", Body: emptyBody()})
}
