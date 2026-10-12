package businness

// indexPlainText_test.go — pins the decision that a doc body is indexed as PLAIN
// TEXT rather than as markup.
//
// WHY IT MATTERS. A doc body is the only content OneCamp ever held as raw HTML on
// its way to OpenSearch, and markup is how an image gets into a text index: an
// <img>, <svg> or <video> element carries its payload in an attribute, so indexing
// the markup indexes the picture. Extracting text first drops every tag and so
// every attribute, which closes the door at the source.
//
// The generic sanitiser in models/openSearch also strips media, so this is
// defence in depth rather than the only guard — but it is the difference between
// "media is removed from the body" and "media never reaches the body", and it is
// also what keeps matching sane (with markup indexed, searching for "div" matched
// every document in the workspace).
//
// The indexed body is checked on the entry itself (domain/Doc DocSearchEntryOf,
// which every write of a doc's entry goes through). The snippet is still checked in the
// source of UpdateDoc, which needs Dgraph to run.

import (
	"os"
	"regexp"
	"strings"
	"testing"

	domain "github.com/akashc777/OneCamp/domain/Doc"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// snippetAssignment matches the stored doc preview. The snippet is indexed as
// doc_snippet AND shown to users in doc lists and search results, so it has the
// same requirement for a second reason: built from raw markup it previewed an
// inlined image as a block of base64, and the cut landed mid-tag.
var snippetAssignment = regexp.MustCompile(`dgraphDoc\.Snippet\s*=\s*((?s:.*?))\n\t\}`)

// commentLine strips whole-line and trailing comments so a code example inside a
// comment cannot satisfy (or break) the check.
var commentLine = regexp.MustCompile(`(?m)//.*$`)

func TestDocBodyIsIndexedAsPlainTextNotMarkup(t *testing.T) {
	body := `<div><h1>Launch plan</h1><p>Ship on <b>Friday</b>.</p>` +
		`<img src="data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB" alt="chart">` +
		`<svg><path d="M0 0L10 10"/></svg></div>`
	e := domain.DocSearchEntryOf(&dgraphStruct.DgraphDoc{Uuid: "d", Title: "Launch", Body: body})
	for _, markup := range []string{"<", ">", "div", "data:image", "base64", "M0 0L10"} {
		if strings.Contains(e.DocBody, markup) {
			t.Errorf("the indexed body carries markup or a payload (%q): %q.\n"+
				"Indexing markup puts image payloads (in src/d attributes) into OpenSearch "+
				"and makes every document match a search for \"div\".", markup, e.DocBody)
		}
	}
	for _, words := range []string{"Launch plan", "Ship on", "Friday"} {
		if !strings.Contains(e.DocBody, words) {
			t.Errorf("the indexed body lost the doc's words %q: %q", words, e.DocBody)
		}
	}
	if strings.Contains(e.DocSnippet, "<") {
		t.Errorf("a doc read without its stored snippet previews markup: %q", e.DocSnippet)
	}
}

func TestDocSnippetIsBuiltFromPlainText(t *testing.T) {
	raw, err := os.ReadFile("docBusiness.go")
	if err != nil {
		t.Fatalf("read docBusiness.go: %v", err)
	}
	src := commentLine.ReplaceAllString(string(raw), "")

	matches := snippetAssignment.FindAllStringSubmatch(src, -1)
	if len(matches) == 0 {
		t.Fatal("no assignment to dgraphDoc.Snippet found; this test has gone stale " +
			"and needs updating to match the current code")
	}

	for _, m := range matches {
		rhs := strings.TrimSpace(m[1])
		if !strings.Contains(rhs, "HTMLToPlainText(") {
			t.Errorf("the doc snippet is built from raw markup:\n    dgraphDoc.Snippet = %s\n\n"+
				"Build it from extracted text:\n"+
				"    dgraphDoc.Snippet = helpers.TruncateRunes(helpers.HTMLToPlainText(body), docSnippetRunes)\n\n"+
				"A snippet taken from raw markup previews an inlined image as base64 and "+
				"can be cut mid-tag.", rhs)
		}
		if !strings.Contains(rhs, "TruncateRunes(") {
			t.Errorf("the doc snippet is truncated without counting characters:\n"+
				"    dgraphDoc.Snippet = %s\n\n"+
				"Use helpers.TruncateRunes so a multi-byte character is never split "+
				"(a byte slice emits invalid UTF-8).", rhs)
		}
	}
}
