package models

// indexBody.go — the single seam every OpenSearch write goes through, and the
// place that guarantees no document can be big enough to hurt the cluster.
//
// WHY THIS EXISTS. A beta search node was OOM-killed by ONE update request. The
// document was a OneCamp doc whose body contained an image inlined as a base64
// data URI, because the doc editor used to fall back to embedding an image when
// its upload was refused. That body was assigned verbatim into the indexed
// document and posted to _update, where OpenSearch buffers the raw request, then
// RE-SERIALISES the doc field into a fresh builder whose backing array grows by
// doubling — so parsing needs several times the payload size, in one contiguous
// allocation, and none of it is covered by the request circuit breakers. The node
// died rather than returning an error, and OpenSearch exits the JVM on OOM.
//
// Four separate limits could have stopped it and none applied: the upload cap
// never saw the bytes (they arrived as text inside a document, not as a file), the
// request-body middleware was not on the doc route, nothing truncated before
// indexing, and OpenSearch's own 100 MB default is far more than a small heap can
// parse. The editor bug is fixed at source, but a search index must not depend on
// every current and future caller being careful — so the guarantee lives HERE,
// where the bytes actually leave for the cluster.
//
// The rule is deliberately generic: it walks any document shape, so it protects
// every index (docs, posts, chats, comments, tasks, attachments, boards, users)
// including ones added later, with no per-model work and nothing to forget.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// MaxIndexedFieldBytes caps ONE string value. Generous for real prose — a very
	// long document still indexes its first ~32 KB, which is far more than search
	// relevance needs — while making a single field incapable of carrying a
	// payload. With this cap a document is bounded by (fields x cap), which for
	// every OneCamp index is well under a megabyte, so no total-size failure path
	// is needed and a document is never rejected outright.
	MaxIndexedFieldBytes = 32 << 10

	// truncationNote marks a clipped value so a reader of the index (or of a search
	// result) can tell truncation from data loss.
	truncationNote = "… [truncated for indexing]"

	// embeddedDataNote replaces an inline data payload. The surrounding text stays
	// searchable; only the payload goes.
	embeddedDataNote = "[embedded-data]"
	// embeddedImageNote replaces an inline image element (see inlineSVGRe).
	embeddedImageNote = "[embedded-image]"
	// minBareBase64Run is the length at which an unbroken run of base64 characters
	// stops being plausible content and starts being a payload. Deliberately far
	// above anything OneCamp stores as a single token — object keys, UUIDs and
	// hashes are tens of characters, not hundreds.
	minBareBase64Run = 512
)

// dataURIRe matches an inline `data:` URI carrying a base64-ish payload — the
// shape an editor produces when it embeds an image instead of uploading it.
//
// Anchored on a payload of at least 32 characters so short, legitimate data URIs
// (a tiny inline SVG marker, a percent-encoded text fragment) are left alone; it
// is size that is dangerous here, not the scheme. The mime/parameter section
// excludes commas and whitespace so the match cannot run past the URI.
var dataURIRe = regexp.MustCompile(`(?i)data:[^,\s"']{0,200},[A-Za-z0-9+/=]{32,}`)

// inlineSVGRe matches an inline <svg> element. An inline SVG *is* an image —
// vector rather than raster — so it is media even though it arrives as markup and
// carries no base64 anywhere. Its path data lives in `d="…"` attributes that
// routinely run to tens of kilobytes, which is exactly the shape that hurt the
// cluster, so a data-URI-only rule would miss it.
//
// Non-greedy, and (?s) so the element may span newlines. A nested <svg> leaves the
// outer closing tag behind as harmless stray text; no image data survives either
// way, which is the property that matters.
var inlineSVGRe = regexp.MustCompile(`(?is)<svg\b[^>]*>.*?</svg\s*>`)

// bareBase64Re matches a long unbroken run of base64 characters that is NOT part of
// a `data:` URI — an image payload pasted or assembled as raw text, which is how
// media slips past a rule that only knows about data URIs.
//
// The base64 alphabet contains no '.', '?', '&' or '-', so hostnames, signed URLs
// and JWTs (dot-separated, each segment short) cannot match a run this long.
// The bound is derived from the constant so the two cannot drift apart.
var bareBase64Re = regexp.MustCompile(`[A-Za-z0-9+/]{` + strconv.Itoa(minBareBase64Run) + `,}={0,2}`)

// SanitizeIndexedText makes one string safe to index. It removes embedded media in
// every form text can smuggle it — `data:` URIs, inline <svg> elements, and raw
// base64 runs — replacing each with a marker, then caps the result at
// MaxIndexedFieldBytes on a UTF-8 rune boundary so a multi-byte character is never
// cut in half (which would produce invalid UTF-8 and be rejected by the cluster).
//
// Surrounding prose is always preserved: only the payload goes, so the document
// stays searchable by the words around the image.
//
// Pure and idempotent: sanitising an already-sanitised value changes nothing.
func SanitizeIndexedText(s string) string {
	if s == "" {
		return s
	}
	// Media stripping runs unconditionally rather than behind a size check, because
	// media is not only a size problem: a small inline image is still an image in a
	// text index, it matches nothing anyone searched for, and the field it sits in
	// may be returned to a client. The three rules cover the three shapes media
	// takes in text — a data URI, an inline SVG element, and a raw base64 run — and
	// the cost of three RE2 scans is nothing beside the HTTP call that follows.
	s = dataURIRe.ReplaceAllString(s, embeddedDataNote)
	s = inlineSVGRe.ReplaceAllString(s, embeddedImageNote)
	s = bareBase64Re.ReplaceAllString(s, embeddedDataNote)
	if len(s) <= MaxIndexedFieldBytes {
		return s
	}
	// Clip to the cap, then back off to the last valid rune boundary.
	clipped := s[:MaxIndexedFieldBytes]
	for len(clipped) > 0 && !utf8.ValidString(clipped) {
		clipped = clipped[:len(clipped)-1]
	}
	return clipped + truncationNote
}

// sanitizeIndexValue walks a decoded JSON value and applies SanitizeIndexedText to
// every string leaf, leaving structure, numbers, booleans and nulls untouched.
// Generic by construction: it knows nothing about any particular document.
func sanitizeIndexValue(v any) any {
	switch t := v.(type) {
	case string:
		return SanitizeIndexedText(t)
	case map[string]any:
		for k, inner := range t {
			t[k] = sanitizeIndexValue(inner)
		}
		return t
	case []any:
		for i, inner := range t {
			t[i] = sanitizeIndexValue(inner)
		}
		return t
	default:
		// Numbers arrive as json.Number (see IndexBody) and re-marshal exactly as
		// they were written, so a unix timestamp cannot turn into 1.23e+09.
		return v
	}
}

// SanitizeIndexedJSON applies the rule to an already-marshalled document, returning
// the cleaned JSON.
//
// Numbers are decoded with UseNumber so they survive the round trip byte-for-byte;
// decoding into interface{} without it would turn every integer into a float64 and
// re-marshal large timestamps in scientific notation, corrupting every date field
// in the index.
func SanitizeIndexedJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		return nil, err
	}
	// Refuse a body holding more than one JSON value. A bulk (NDJSON) body looks
	// like several values separated by newlines, and Decode reads only the FIRST —
	// so without this check, passing a bulk body here would return one document and
	// silently discard every other operation in the batch. Failing loudly sends the
	// caller to SanitizeIndexedNDJSON, which is the function that handles that shape.
	if dec.More() {
		return nil, errMultipleJSONValues
	}
	return json.Marshal(sanitizeIndexValue(decoded))
}

// errMultipleJSONValues signals that a body contained more than one JSON value and
// therefore needs the NDJSON-aware path.
var errMultipleJSONValues = errors.New(
	"models/openSearch: body contains multiple JSON values; use SanitizeIndexedNDJSON for bulk bodies")

// SanitizeIndexedNDJSON applies the rule to a BULK body.
//
// A bulk request is newline-delimited JSON: an action line ({"update":{…}}), then
// the document line, repeated. The line structure is load-bearing — OpenSearch
// pairs each action with the line after it and requires a trailing newline — so
// each line is sanitised independently and the layout is reproduced exactly.
//
// A line that does not parse is passed through untouched rather than dropped, for
// the same reason IndexReader falls back to raw bytes: losing operations from a
// batch would be a worse failure than an unsanitised value getting through.
func SanitizeIndexedNDJSON(raw string) string {
	if raw == "" {
		return raw
	}
	// Split/Join on "\n" reproduces the input's line layout exactly, including the
	// trailing newline that OpenSearch requires (it becomes a final empty element).
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		cleaned, err := SanitizeIndexedJSON([]byte(line))
		if err != nil {
			continue
		}
		lines[i] = string(cleaned)
	}
	return strings.Join(lines, "\n")
}

// BulkReader is the seam for bulk writes, mirroring IndexReader for single
// documents: every bulk body goes through here so the guarantee covers batched
// writes too, which is most of the write volume (posts and comments with
// attachments, and every cascading rename or delete).
func BulkReader(bulk string) io.Reader {
	return strings.NewReader(SanitizeIndexedNDJSON(bulk))
}

// IndexReader is the seam every model uses in place of
// openSearchStruct.IndexReader(jsonData): it sanitises the marshalled document and
// returns a reader over the result.
//
// It cannot fail from the caller's point of view. If the bytes somehow do not parse
// it returns them unchanged rather than dropping the write — the input has just been
// produced by json.Marshal, so that path is unreachable in practice, and silently
// losing a document from the search index would be a worse outcome than an
// unsanitised one slipping through. The cluster-side cap
// (http.max_content_length) is the backstop for that impossible case.
func IndexReader(raw []byte) io.Reader {
	cleaned, err := SanitizeIndexedJSON(raw)
	if err != nil {
		return bytes.NewReader(raw)
	}
	return bytes.NewReader(cleaned)
}
