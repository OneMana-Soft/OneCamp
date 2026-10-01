package models

// mediaGuard_test.go — pins the guarantee that NO media (image, video, audio, or
// any binary payload) can be stored in OpenSearch.
//
// There are two independent ways media could get in, so there are two kinds of
// test here:
//
//  1. STRUCTURALLY, if an indexed struct grew a field capable of holding bytes.
//     TestNoOpenSearchFieldCanCarryBinary parses struct.go and fails on any such
//     field, so the guarantee cannot be lost by adding a field.
//
//  2. AS TEXT, if a string field carried media smuggled inside it. That is what
//     actually happened in production (a base64 image inlined in a doc body), and
//     it has three shapes: a `data:` URI, an inline <svg> element, and a raw
//     base64 run. Each gets a test, plus a test that ordinary prose and real
//     image *references* (URLs, object keys) survive untouched — a guard that
//     also ate legitimate content would just be a different bug.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// --- 1. Structural: no indexed field may hold bytes ------------------------

// TestNoOpenSearchFieldCanCarryBinary fails if any field of any indexed struct has
// a type that can hold raw bytes. Media has to be *representable* before it can be
// stored, so denying the representation is the strongest available guarantee: it
// holds for every index, including ones added later, without anyone remembering
// this file exists.
func TestNoOpenSearchFieldCanCarryBinary(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "struct.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse struct.go: %v", err)
	}

	// Types that can carry a binary payload. []byte is the obvious one; the others
	// are the shapes a well-meaning change might reach for instead.
	banned := map[string]string{
		"[]byte":    "a byte slice can hold a whole file",
		"[]uint8":   "[]byte by another name",
		"[][]byte":  "a slice of byte slices can hold several files",
		"io.Reader": "a reader streams arbitrary bytes into the document",
	}

	var offences []string
	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, field := range st.Fields.List {
			rendered := renderType(field.Type)
			if reason, bad := banned[rendered]; bad {
				for _, name := range field.Names {
					offences = append(offences, ts.Name.Name+"."+name.Name+
						" is "+rendered+" — "+reason)
				}
			}
		}
		return true
	})

	if len(offences) > 0 {
		t.Errorf("indexed struct fields can carry binary data, so media could be "+
			"stored in OpenSearch:\n  %s\n\nOpenSearch indexes text for searching. "+
			"Store the file in object storage and index its metadata (filename, "+
			"object key) instead, the way OpenSearchAttachment does.",
			strings.Join(offences, "\n  "))
	}
}

// renderType returns a comparable string for the simple type expressions that
// appear in struct.go. Anything it does not recognise renders as "" and is
// therefore not matched against the ban list, which is the safe direction: the
// test can only fail on a type it positively identified.
func renderType(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.ArrayType:
		if t.Len != nil {
			return ""
		}
		return "[]" + renderType(t.Elt)
	case *ast.SelectorExpr:
		pkg, ok := t.X.(*ast.Ident)
		if !ok {
			return ""
		}
		return pkg.Name + "." + t.Sel.Name
	default:
		return ""
	}
}

// TestAttachmentIndexStoresMetadataOnly documents and pins the design decision
// that makes attachments safe: the attachment index describes a file, it does not
// contain one. If a content-bearing field ever appears here, this fails.
func TestAttachmentIndexStoresMetadataOnly(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "struct.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse struct.go: %v", err)
	}

	// Field names that would mean "the bytes of the file are in here". Note that
	// AttachmentObjKey and AttachmentFileName are references, not content, and are
	// deliberately absent from this list.
	contentish := []string{
		"attachmentdata", "attachmentcontent", "attachmentbytes",
		"attachmentbody", "attachmentblob", "attachmentbase64",
		"attachmentthumbnail", "attachmentpreview", "attachmentraw",
	}

	var offences []string
	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "OpenSearchAttachment" {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, field := range st.Fields.List {
			for _, name := range field.Names {
				lower := strings.ToLower(name.Name)
				for _, bad := range contentish {
					if lower == bad {
						offences = append(offences, name.Name)
					}
				}
			}
		}
		return false
	})

	if len(offences) > 0 {
		t.Errorf("OpenSearchAttachment gained content-bearing field(s) %v; the "+
			"attachment index must describe files, not carry them",
			offences)
	}
}

// --- 2. As text: media smuggled inside a string ---------------------------

func TestSanitizeIndexedText_StripsInlineSVG(t *testing.T) {
	// An inline SVG is an image with no base64 anywhere in it, which is precisely
	// the case a data-URI-only rule misses.
	svg := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100">` +
		`<path d="` + strings.Repeat("M10 10 L90 90 ", 2000) + `"/></svg>`
	body := "Quarterly plan " + svg + " approved by finance"

	got := SanitizeIndexedText(body)

	if strings.Contains(got, "<svg") || strings.Contains(got, "<path") {
		t.Errorf("inline SVG survived sanitising: %.200q", got)
	}
	if strings.Contains(got, "M10 10 L90 90") {
		t.Error("SVG path data survived sanitising, so the image is still indexed")
	}
	// The words around the image are why the document is searchable at all.
	if !strings.Contains(got, "Quarterly plan") || !strings.Contains(got, "approved by finance") {
		t.Errorf("prose around the image was lost: %q", got)
	}
	if !strings.Contains(got, embeddedImageNote) {
		t.Errorf("removal was not marked, so a reader cannot tell it from data loss: %q", got)
	}
}

func TestSanitizeIndexedText_StripsSVGSpanningNewlines(t *testing.T) {
	// Editors pretty-print markup, so the element routinely contains newlines. A
	// pattern without (?s) would silently miss every one of them.
	svg := "<svg viewBox=\"0 0 10 10\">\n  <circle cx=\"5\" cy=\"5\" r=\"4\"/>\n</svg>"

	got := SanitizeIndexedText("before " + svg + " after")

	if strings.Contains(got, "<circle") {
		t.Errorf("multi-line SVG survived sanitising: %q", got)
	}
}

func TestSanitizeIndexedText_StripsRawBase64Runs(t *testing.T) {
	// Base64 with the `data:` prefix stripped off, which is what arrives when
	// media is assembled or pasted as plain text rather than as a URI.
	payload := strings.Repeat("iVBORw0KGgoAAAANSUhEUg", 60) // ~1.3 KB, no "data:"
	body := "see attached " + payload + " end"

	got := SanitizeIndexedText(body)

	if strings.Contains(got, payload) {
		t.Errorf("raw base64 payload survived sanitising: %.200q", got)
	}
	if !strings.Contains(got, "see attached") || !strings.Contains(got, "end") {
		t.Errorf("surrounding text was lost: %q", got)
	}
}

func TestSanitizeIndexedText_KeepsImageReferences(t *testing.T) {
	// The point is to store references INSTEAD of media, so references must
	// survive: strip these and search stops being able to find the attachment.
	for _, ref := range []string{
		`<img src="https://cdn.onecamp.dev/w/abc123/screenshot.png" alt="login screen">`,
		"attachments/2026/07/31/9f3c1e2a-4b5d-11ee-be56-0242ac120002/design-review.png",
		"See the diagram at https://onecamp.dev/app/doc/abcd-1234?highlight=budget",
		"logo.svg",
		"data:image/svg+xml,<svg/>",
	} {
		if got := SanitizeIndexedText(ref); got != ref {
			t.Errorf("image REFERENCE was altered, breaking search for it:\n  in:  %q\n  out: %q", ref, got)
		}
	}
}

func TestSanitizeIndexedText_LeavesCodeAndIdentifiersAlone(t *testing.T) {
	// The base64 rule keys on run length, so anything shorter than the floor must
	// pass through untouched — including the long-ish tokens OneCamp really stores.
	for _, s := range []string{
		"9f3c1e2a-4b5d-11ee-be56-0242ac120002",
		"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk",
		"if (a < b) { return c > d; }",
		strings.Repeat("A", minBareBase64Run-1),
	} {
		if got := SanitizeIndexedText(s); got != s {
			t.Errorf("ordinary value was altered:\n  in:  %.80q\n  out: %.80q", s, got)
		}
	}
}

func TestSanitizeIndexedText_MediaStrippingIsIdempotent(t *testing.T) {
	// Documents are re-indexed on every edit, so sanitising a value that has
	// already been sanitised must be a no-op. If a marker were itself matched by
	// one of the rules, markers would accumulate on every save.
	body := `<svg><path d="` + strings.Repeat("M1 1 ", 3000) + `"/></svg> and ` +
		strings.Repeat("iVBORw0KGgoAAAANSUhEUg", 60)

	once := SanitizeIndexedText(body)
	twice := SanitizeIndexedText(once)

	if once != twice {
		t.Errorf("sanitising is not idempotent, so markers accumulate on re-index:\n  once:  %.160q\n  twice: %.160q", once, twice)
	}
}

func TestIndexReader_StripsMediaAnywhereInTheDocument(t *testing.T) {
	// The end-to-end property: whatever the shape of the document, no media
	// reaches the reader that goes to the cluster.
	doc := map[string]any{
		"doc_title": `<svg><path d="` + strings.Repeat("M2 2 ", 3000) + `"/></svg>`,
		"doc_body":  "prose " + strings.Repeat("iVBORw0KGgoAAAANSUhEUg", 60),
		"nested": map[string]any{
			"comment_body": `<img src="data:image/png;base64,` + strings.Repeat("A", 5000) + `">`,
		},
		"list": []any{`<svg viewBox="0 0 9 9"><rect width="9" height="9"/></svg>`},
	}

	body, err := IndexBody(doc)
	if err != nil {
		t.Fatalf("IndexBody: %v", err)
	}
	buf := make([]byte, 1<<20)
	n, _ := body.Read(buf)
	out := string(buf[:n])

	for _, forbidden := range []string{"<svg", "<rect", "<path", "base64,", "iVBORw0KGgoAAAANSUhEUg"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("media marker %q reached the cluster payload: %.300q", forbidden, out)
		}
	}
	if !strings.Contains(out, "prose") {
		t.Errorf("real content was lost: %.300q", out)
	}
}
