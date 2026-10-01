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
// This reads source rather than calling UpdateDoc because that function needs
// Dgraph, Postgres and OpenSearch to run. The assertion is narrow and specific, so
// it is still a real check: it fails if the extraction call is removed.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// docBodyAssignment matches the assignment of the indexed doc body, capturing
// whatever is on the right-hand side.
var docBodyAssignment = regexp.MustCompile(`openSearchDoc\.DocBody\s*=\s*([^\n]+)`)

// snippetAssignment matches the stored doc preview. The snippet is indexed as
// doc_snippet AND shown to users in doc lists and search results, so it has the
// same requirement for a second reason: built from raw markup it previewed an
// inlined image as a block of base64, and the cut landed mid-tag.
var snippetAssignment = regexp.MustCompile(`dgraphDoc\.Snippet\s*=\s*((?s:.*?))\n\t\}`)

// commentLine strips whole-line and trailing comments so a code example inside a
// comment cannot satisfy (or break) the check.
var commentLine = regexp.MustCompile(`(?m)//.*$`)

func TestDocBodyIsIndexedAsPlainTextNotMarkup(t *testing.T) {
	raw, err := os.ReadFile("docBusiness.go")
	if err != nil {
		t.Fatalf("read docBusiness.go: %v", err)
	}
	src := commentLine.ReplaceAllString(string(raw), "")

	matches := docBodyAssignment.FindAllStringSubmatch(src, -1)
	if len(matches) == 0 {
		t.Fatal("no assignment to openSearchDoc.DocBody found; this test has gone " +
			"stale and needs updating to match the current code")
	}

	for _, m := range matches {
		rhs := strings.TrimSpace(m[1])

		// Assigning the empty string is fine: that is the create path, which
		// indexes a doc before it has a body.
		if rhs == `""` {
			continue
		}

		if !strings.Contains(rhs, "HTMLToPlainText(") {
			t.Errorf("openSearchDoc.DocBody is assigned raw markup:\n"+
				"    openSearchDoc.DocBody = %s\n\n"+
				"Index extracted text instead:\n"+
				"    openSearchDoc.DocBody = helpers.HTMLToPlainText(...)\n\n"+
				"Indexing markup puts image payloads (in src/d attributes) into "+
				"OpenSearch and makes every document match a search for \"div\".", rhs)
		}
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
