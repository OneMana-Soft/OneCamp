package models

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"unicode/utf8"
)

// indexBody_test.go — the guarantee that no document leaving for OpenSearch can be
// large enough to hurt it. The failure being prevented is concrete: one update
// request carrying a base64 image in a doc body exhausted a search node's heap and
// stopped the container.

func decodeBody(t *testing.T, r io.Reader) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("body must be valid JSON: %v\n%s", uerr, raw)
	}
	return out
}

// THE KEY CASE: an inline base64 image is removed, and the prose around it stays
// searchable.
func TestSanitizeIndexedText_StripsInlineBase64Payloads(t *testing.T) {
	payload := strings.Repeat("A", 4000)
	in := `<p>Quarterly review</p><img src="data:image/png;base64,` + payload + `"><p>See the chart above.</p>`

	got := SanitizeIndexedText(in)

	if strings.Contains(got, payload[:64]) {
		t.Fatalf("the base64 payload must not survive indexing; got %q", got)
	}
	if !strings.Contains(got, embeddedDataNote) {
		t.Fatalf("the removal must be marked so it reads as truncation, not loss; got %q", got)
	}
	for _, want := range []string{"Quarterly review", "See the chart above."} {
		if !strings.Contains(got, want) {
			t.Errorf("surrounding text must stay searchable, lost %q from %q", want, got)
		}
	}
	if len(got) >= len(in) {
		t.Fatalf("sanitising must shrink a payload-bearing value: %d -> %d", len(in), len(got))
	}
}

// Several images in one document — the realistic case, since a person pastes more
// than one and may retry after a silent failure.
func TestSanitizeIndexedText_StripsEveryPayload(t *testing.T) {
	one := `<img src="data:image/jpeg;base64,` + strings.Repeat("B", 2000) + `">`
	in := "start" + one + "middle" + one + "end"
	got := SanitizeIndexedText(in)
	if strings.Contains(got, "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB") {
		t.Fatalf("every payload must be removed, not just the first; got %q", got)
	}
	for _, want := range []string{"start", "middle", "end"} {
		if !strings.Contains(got, want) {
			t.Errorf("lost %q", want)
		}
	}
}

// Size is what is dangerous, not the scheme: a tiny inline marker is harmless and
// must not be mangled.
func TestSanitizeIndexedText_LeavesShortDataURIsAlone(t *testing.T) {
	in := `<img src="data:image/gif;base64,R0lGODlhAQABAA==">`
	if got := SanitizeIndexedText(in); got != in {
		t.Fatalf("a short data URI must pass through unchanged;\n in: %q\nout: %q", in, got)
	}
}

func TestSanitizeIndexedText_CapsOversizedValues(t *testing.T) {
	// Real prose, not a repeated character: an unbroken run of base64-alphabet
	// characters this long is treated as a payload and replaced (see
	// bareBase64Re), which is correct but would not exercise the cap.
	in := strings.Repeat("lorem ipsum dolor sit amet ", (MaxIndexedFieldBytes*3)/27)
	got := SanitizeIndexedText(in)
	if len(got) > MaxIndexedFieldBytes+len(truncationNote) {
		t.Fatalf("a value must be capped; got %d bytes", len(got))
	}
	if !strings.HasSuffix(got, truncationNote) {
		t.Fatalf("truncation must be disclosed; got tail %q", got[max(0, len(got)-40):])
	}
}

// Cutting a multi-byte character in half produces invalid UTF-8, which the cluster
// rejects — so the clip has to land on a rune boundary.
func TestSanitizeIndexedText_NeverEmitsInvalidUTF8(t *testing.T) {
	// Every rune is 3 bytes, so a naive byte clip will land mid-character.
	in := strings.Repeat("あ", MaxIndexedFieldBytes)
	got := SanitizeIndexedText(in)
	if !utf8.ValidString(got) {
		t.Fatal("a truncated value must remain valid UTF-8")
	}
	if len(got) > MaxIndexedFieldBytes+len(truncationNote) {
		t.Fatalf("still must respect the cap; got %d", len(got))
	}
}

func TestSanitizeIndexedText_IsIdempotent(t *testing.T) {
	in := `body <img src="data:image/png;base64,` + strings.Repeat("C", 3000) + `"> ` + strings.Repeat("y", MaxIndexedFieldBytes)
	once := SanitizeIndexedText(in)
	if twice := SanitizeIndexedText(once); twice != once {
		t.Fatalf("sanitising twice must not change the result:\n1: %q\n2: %q", once[:80], twice[:80])
	}
}

func TestSanitizeIndexedText_LeavesOrdinaryTextUntouched(t *testing.T) {
	for _, in := range []string{
		"",
		"A normal document body with some words in it.",
		"https://cdn.example.com/uploads/a1b2c3/photo.png",
		"user@example.com",
	} {
		if got := SanitizeIndexedText(in); got != in {
			t.Errorf("ordinary text must pass through;\n in: %q\nout: %q", in, got)
		}
	}
}

// IndexBody must apply the rule to whatever shape it is handed, including nested
// objects and arrays, so an index added later is protected without any new work.
func TestIndexBody_SanitizesEveryStringAtEveryDepth(t *testing.T) {
	payload := strings.Repeat("D", 3000)
	doc := map[string]any{
		"doc_uuid": "abc-123",
		"doc_body": `<img src="data:image/png;base64,` + payload + `">`,
		"nested": map[string]any{
			"deep": `x data:image/png;base64,` + payload,
		},
		"list": []any{
			`y data:image/png;base64,` + payload,
			"clean value",
		},
	}
	r, err := IndexBody(doc)
	if err != nil {
		t.Fatal(err)
	}
	out := decodeBody(t, r)

	if s, _ := out["doc_body"].(string); strings.Contains(s, payload[:64]) {
		t.Error("top-level string not sanitised")
	}
	nested, _ := out["nested"].(map[string]any)
	if s, _ := nested["deep"].(string); strings.Contains(s, payload[:64]) {
		t.Error("nested object string not sanitised")
	}
	list, _ := out["list"].([]any)
	if s, _ := list[0].(string); strings.Contains(s, payload[:64]) {
		t.Error("array element not sanitised")
	}
	if s, _ := list[1].(string); s != "clean value" {
		t.Errorf("a clean array element must be untouched, got %q", s)
	}
	if out["doc_uuid"] != "abc-123" {
		t.Errorf("identifiers must be untouched, got %v", out["doc_uuid"])
	}
}

// Round-tripping through interface{} without UseNumber turns integers into float64
// and re-marshals large values in scientific notation, which would corrupt every
// unix-timestamp field in the index.
func TestIndexBody_PreservesNumbersExactly(t *testing.T) {
	doc := map[string]any{
		"created_date": int64(1767225600),
		"updated_date": int64(1767225600123),
		"count":        0,
		"ratio":        1.5,
		"deleted_date": nil,
		"flag":         true,
	}
	r, err := IndexBody(doc)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(r)
	body := string(raw)

	for _, want := range []string{`"created_date":1767225600`, `"updated_date":1767225600123`, `"count":0`, `"ratio":1.5`, `"deleted_date":null`, `"flag":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %s in %s", want, body)
		}
	}
	if strings.Contains(body, "e+") || strings.Contains(body, "E+") {
		t.Fatalf("no number may be re-marshalled in scientific notation: %s", body)
	}
}

// The bound that matters: whatever a caller hands over, the request that reaches
// the cluster is small. A struct with several oversized fields is the realistic
// worst case.
func TestIndexBody_BoundsTheWholeDocument(t *testing.T) {
	huge := strings.Repeat("z", 20<<20) // 20 MB per field
	doc := map[string]any{
		"doc_body":    huge,
		"doc_title":   huge,
		"doc_snippet": huge,
	}
	r, err := IndexBody(doc)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(r)
	if len(raw) > 4*(MaxIndexedFieldBytes+len(truncationNote)+64) {
		t.Fatalf("the indexed document must be bounded; got %d bytes from 60 MB of input", len(raw))
	}
	// And it must still be usable, not mangled.
	decodeBody(t, strings.NewReader(string(raw)))
}

func TestIndexBody_RejectsUnmarshalableInput(t *testing.T) {
	if _, err := IndexBody(map[string]any{"bad": func() {}}); err == nil {
		t.Fatal("a value that cannot be marshalled must surface an error, not a silent empty body")
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// --- Bulk (NDJSON) bodies --------------------------------------------------
//
// The bulk path carries most of OneCamp's write volume and is assembled by hand in
// the domain layer, so it needs the same guarantee as a single document — but its
// line structure is load-bearing and must survive exactly.

func TestSanitizeIndexedJSON_RefusesMultipleValues(t *testing.T) {
	// This is the most dangerous mistake available in this file. json.Decoder.Decode
	// reads ONE value, so sanitising a bulk body with the single-document function
	// would return the first operation and silently discard all the rest. It has to
	// fail instead.
	bulk := `{"update":{"_index":"posts","_id":"a"}}` + "\n" + `{"doc":{"post_body":"hello"}}` + "\n"

	if _, err := SanitizeIndexedJSON([]byte(bulk)); err == nil {
		t.Fatal("SanitizeIndexedJSON accepted a multi-value (bulk) body; it would have " +
			"returned only the first operation and dropped the rest of the batch")
	}
}

func TestSanitizeIndexedNDJSON_PreservesLineStructureExactly(t *testing.T) {
	// OpenSearch pairs each action line with the line that follows it and requires a
	// trailing newline. Lose either property and the whole batch is rejected or,
	// worse, misapplied.
	in := `{"update":{"_index":"posts","_id":"p1"}}` + "\n" +
		`{"doc":{"post_body":"first"},"doc_as_upsert":true}` + "\n" +
		`{"update":{"_index":"attachments","_id":"a1"}}` + "\n" +
		`{"doc":{"attachment_file_name":"shot.png"}}` + "\n"

	got := SanitizeIndexedNDJSON(in)

	if strings.Count(got, "\n") != strings.Count(in, "\n") {
		t.Errorf("line count changed: in %d newlines, out %d\n%q",
			strings.Count(in, "\n"), strings.Count(got, "\n"), got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Error("bulk body lost its trailing newline, which OpenSearch requires")
	}
	// Action lines must still address the same documents. Compared as parsed JSON,
	// not as bytes: the round trip through a map re-orders object keys, which is
	// meaningless to OpenSearch (JSON objects are unordered) but would make a
	// byte-level assertion fail for no reason.
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d:\n%s", len(lines), got)
	}
	for i, want := range []struct{ index, id string }{
		{"posts", "p1"},
		{"attachments", "a1"},
	} {
		var action map[string]map[string]string
		if err := json.Unmarshal([]byte(lines[i*2]), &action); err != nil {
			t.Fatalf("action line %d no longer parses: %v\n%s", i*2, err, lines[i*2])
		}
		if action["update"]["_index"] != want.index || action["update"]["_id"] != want.id {
			t.Errorf("action line %d now addresses a different document: %v (want %s/%s)",
				i*2, action, want.index, want.id)
		}
	}
	// And the payload must survive, including the upsert flag.
	if !strings.Contains(got, `"first"`) || !strings.Contains(got, "doc_as_upsert") {
		t.Errorf("document content was lost:\n%s", got)
	}
}

func TestSanitizeIndexedNDJSON_StripsMediaFromDocumentLines(t *testing.T) {
	payload := strings.Repeat("A", 4000)
	in := `{"update":{"_index":"posts","_id":"p1"}}` + "\n" +
		`{"doc":{"post_body":"look <img src=\"data:image/png;base64,` + payload + `\">"}}` + "\n"

	got := SanitizeIndexedNDJSON(in)

	if strings.Contains(got, payload) {
		t.Errorf("embedded media survived in a bulk body: %.200q", got)
	}
	if !strings.Contains(got, "look") {
		t.Errorf("surrounding content was lost: %q", got)
	}
	if !strings.Contains(got, `"_id":"p1"`) {
		t.Errorf("action line was damaged while sanitising: %q", got)
	}
}

func TestSanitizeIndexedNDJSON_PassesThroughUnparseableLines(t *testing.T) {
	// Dropping operations from a batch would be a worse failure than letting one
	// unsanitised value through, so an unparseable line survives unchanged.
	in := "not json at all\n" + `{"doc":{"post_body":"ok"}}` + "\n"

	got := SanitizeIndexedNDJSON(in)

	if !strings.Contains(got, "not json at all") {
		t.Errorf("an unparseable line was dropped instead of passed through: %q", got)
	}
	if !strings.Contains(got, `"ok"`) {
		t.Errorf("the valid line was lost: %q", got)
	}
}

func TestSanitizeIndexedNDJSON_HandlesEmptyAndBlankInput(t *testing.T) {
	if got := SanitizeIndexedNDJSON(""); got != "" {
		t.Errorf("empty input must stay empty; got %q", got)
	}
	if got := SanitizeIndexedNDJSON("\n\n"); got != "\n\n" {
		t.Errorf("blank lines must be preserved; got %q", got)
	}
}

func TestBulkReader_SanitizesTheBodyItReturns(t *testing.T) {
	payload := strings.Repeat("B", 4000)
	in := `{"update":{"_index":"posts","_id":"p1"}}` + "\n" +
		`{"doc":{"post_body":"data:image/png;base64,` + payload + `"}}` + "\n"

	buf := make([]byte, 1<<20)
	n, _ := BulkReader(in).Read(buf)
	out := string(buf[:n])

	if strings.Contains(out, payload) {
		t.Errorf("BulkReader returned an unsanitised body: %.200q", out)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Error("BulkReader dropped the trailing newline OpenSearch requires")
	}
}
