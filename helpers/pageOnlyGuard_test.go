package helpers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The reads that take pages only carry pageOnly (docs F1).
//
// WHY. A folder, a file and a link are doc nodes like a page (domain/Doc
// docNodeKind.go), so every read rooted at "every doc" now meets them too. A
// scan that treats one as a page does something wrong with it: the doc repair
// would rebuild a body nobody wrote, the AI index would embed an empty page,
// the retention would archive a folder of pages nobody edits, the work graph's
// reconciler would read a body for links. Each read below takes pages only,
// through domain/Doc's pageOnly filter in its query or its IsPage check in
// code, and the filter is spelled once (domain/Doc docNodeKind.go), so a read
// that needs it names it. Each line says where and why; a read that stops
// naming what it must fails here, and so does a list line whose declaration
// has gone (rename it here with the read).
type pageOnlyRead struct {
	file, decl string
	// mention is what the declaration's source must name.
	mention string
	why     string
}

var pageOnlyReads = []pageOnlyRead{
	{"domain/Doc/docDomain.go", "GetDocBodyPage", "pageOnly", "the doc repair rebuilds pages' bodies"},
	{"domain/Doc/docDomain.go", "GetDocAccessPage", "pageOnly", "the AI index's access backfill: only pages are embedded"},
	{"domain/Doc/docDomain.go", "inactiveDocFilter", "pageOnly", "the retention archives pages nobody edits; files are kept until someone removes them"},
	{"domain/Doc/docDomain.go", "GetDgraphDocByUUID", "pageOnly", "a page's backlink count counts pages linking to it"},
	{"domain/Doc/docDomain.go", "GetCollabDocByUUID", "doc_node_kind", "the collaboration service's load reads the kind, so it opens pages only"},
	{"domain/Doc/docDomain.go", "GetSystemDocByUUID", "doc_node_kind", "a save's re-embedding reads the kind, so only a page is embedded"},
	{"domain/Doc/docLook.go", "GetDocMentioners", "pageOnly", "the backlinks panel lists the pages linking to a doc"},
	{"domain/Doc/docCollabState.go", "collabUpsertQuery", "pageOnly", "a save from the collaboration service writes a page's body only"},
	{"domain/Doc/searchEntry.go", "DocSearchEntryOf", "IsPage", "a folder's or a link's entry has its title alone"},
	{"domain/Doc/searchEntry.go", "writeDocSearch", "OwnSearchEntry", "a file is found through its versions' entries, never a doc entry"},
	{"business/Doc/searchRepair.go", "writeDocSearchRepairPage", "OwnSearchEntry", "the search repair writes entries as writeDocSearch does"},
	{"business/Doc/docBusiness.go", "updateDoc", "IsPage", "a save re-embeds a page only"},
	{"business/Doc/docCreate.go", "CreateDocInTree", "IsPage", "a new blank doc is embedded only when it is a page"},
	{"business/Doc/docDuplicate.go", "DuplicateDocs", "IsPage", "a copy is embedded only when it is a page"},
	{"business/Doc/docDuplicate.go", "seedsCopy", "IsPage", "a copy's body is seeded into a page only"},
	{"business/Doc/docCollabSave.go", "storedBodyOf", "IsPage", "a stored body is a page's: the seed's fallback never writes a first body into a folder, a file or a link"},
	{"business/AI/docAI.go", "liveDocHTML", "IsPage", "Doc AI reads a page's live body, never a folder's, a file's or a link's"},
	{"business/ContentIO/workspaceExport.go", "exportableDocs", "IsPage", "the workspace export writes pages as Markdown; a folder is a directory only"},
	{"domain/WorkGraph/refs.go", "sourceFilter", "PageOnly", "the work graph's reconciler reads pages' bodies for links (DG6)"},
	{"domain/WorkGraph/refs.go", "ReadSourcesQuery", "sourceBlock", "its page of sources, filtered as sourceBlock filters them"},
	{"domain/WorkGraph/refs.go", "ReadSource", "sourceBlock", "one source, filtered as sourceBlock filters them"},
	{"domain/WorkGraph/refs.go", "ReadSourcesByUUID", "sourceBlock", "sources by id, filtered as sourceBlock filters them"},
	{"domain/WorkGraph/refs.go", "sourceBlock", "sourceFilter", "a source read's root takes its kind's filter"},
	// Who knows credits pages only (DGF1): making a folder, uploading a file
	// or adding a link isn't writing about anything.
	{"domain/WhoKnows/count.go", "kinds", "PageOnly", "Who knows' count takes pages only, on the doc block its evidence reads"},
	{"domain/WhoKnows/count.go", "CountQuery", "k.keep()", "each kind's block takes its kind's filter"},
	{"business/WhoKnows/whoKnows.go", "decide", "NodeKindOf", "a folder, a file or a link is left out before the count and before considered"},
	// Only a page opens in the editor: every way in refuses the rest, and
	// the reads they decide by carry the kind.
	{"domain/Doc/docAccess.go", "accessScalars", "doc_node_kind", "every decision reads the kind, so the live session refuses a non-page with no other read"},
	{"domain/Doc/docAccessRule.go", "DecideDoc", "NodeKind", "the decision carries the kind"},
	{"controllers/Doc/docCollabSessionController.go", "DocCollabSession", "NodeKind", "the live session opens pages only (409)"},
	{"controllers/Doc/docAccessController.go", "AuthorizeDocSocket", "NodeKind", "the older socket authorisation opens pages only (409)"},
	{"controllers/Doc/docController.go", "GetDocForCollab", "IsPage", "the collaboration service loads pages only (409)"},
	{"controllers/Doc/docBodyController.go", "SaveDocFromCollab", "ErrNotAPage", "a save to a non-page is refused (409), the backstop"},
	{"domain/Doc/docDomain.go", "GetDocDeletion", "doc_node_kind", "a guest's live check reads the kind"},
	{"business/Doc/access.go", "CheckPageLive", "IsPage", "a guest's live connection opens pages only"},
	{"controllers/Guest/guestController.go", "GuestDocColabAuthorize", "ErrNotAPage", "the guest authorise route refuses a non-page (409), the backstop"},
}

func TestThePageOnlyReadsCarryIt(t *testing.T) {
	for _, r := range pageOnlyReads {
		text, ok := declSource(t, "../"+r.file, r.decl)
		switch {
		case !ok:
			t.Errorf("%s: no declaration %s; it moved or was renamed, so follow it here (%s)", r.file, r.decl, r.why)
		case !strings.Contains(text, r.mention):
			t.Errorf("%s %s doesn't name %s: %s", r.file, r.decl, r.mention, r.why)
		}
	}
}

// pageOnlySpelling is the filter as domain/Doc writes it, once.
const pageOnlySpelling = "NOT has(doc_node_kind)"

// The filter is spelled in one place, so a read names it rather than writing
// it again.
func TestPageOnlyIsSpelledOnce(t *testing.T) {
	var where []string
	count := 0
	for _, root := range []string{"../business", "../controllers", "../domain", "../models", "../services", "../helpers"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if n := strings.Count(string(b), pageOnlySpelling); n > 0 {
				count += n
				where = append(where, filepath.ToSlash(path))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if count != 1 || len(where) != 1 || where[0] != "../domain/Doc/docNodeKind.go" {
		t.Errorf("%q is spelled %d times, in %v: name domain/Doc's pageOnly (PageOnly outside it) instead", pageOnlySpelling, count, where)
	}
}

// declSource is the source of the top-level function, method, const or var
// named name in the file at path.
func declSource(t *testing.T, path, name string) (string, bool) {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	slice := func(n ast.Node) string {
		return string(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])
	}
	for _, d := range f.Decls {
		switch x := d.(type) {
		case *ast.FuncDecl:
			if x.Name.Name == name {
				return slice(x), true
			}
		case *ast.GenDecl:
			for _, s := range x.Specs {
				if v, ok := s.(*ast.ValueSpec); ok {
					for _, n := range v.Names {
						if n.Name == name {
							return slice(v), true
						}
					}
				}
			}
		}
	}
	return "", false
}
