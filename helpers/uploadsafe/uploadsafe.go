// Package uploadsafe centralises file-upload defences that are
// duplicated across the user/admin/import upload paths:
//
//  1. Magic-byte signature checks. We never trust a client-supplied
//     Content-Type header or filename extension for security
//     decisions; the request body is what counts.
//  2. Safe Content-Type derivation. Files with executable or
//     browser-renderable extensions (.html, .svg, .js, ...) are
//     coerced to application/octet-stream so a presigned MinIO GET
//     never serves them inline.
//  3. Safe Content-Disposition. Every download is forced to
//     `attachment; filename="..."` — the browser saves it instead of
//     rendering. Inline rendering is opt-in for known-safe types.
//  4. Filename sanitisation. Strips path separators, control chars
//     and trims length while keeping the extension and any
//     printable Unicode (so résumé.pdf survives intact).
//
// Every upload entry point in the codebase should pass through
// PrepareUpload() before handing bytes to MinIO. That function returns
// the safe Content-Type, the safe filename, the Content-Disposition
// header value, and a Reader that's been peeked-and-rewound so the
// caller can hand it straight to MinIO.PutObject.
package uploadsafe

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Errors callers can match on.
var (
	ErrEmptyBody         = errors.New("upload body is empty")
	ErrUnsupportedType   = errors.New("uploaded file type is not allowed")
	ErrSignatureMismatch = errors.New("uploaded bytes do not match a recognised file signature")
)

// inlineSafeTypes is the strict allowlist for `Content-Disposition: inline`.
// Anything outside this set is served `attachment` so the browser saves
// it instead of rendering. SVG is deliberately excluded — it can carry
// JavaScript and is a stored-XSS vector.
var inlineSafeTypes = map[string]bool{
	"image/png":                true,
	"image/jpeg":               true,
	"image/gif":                true,
	"image/webp":               true,
	"image/bmp":                true,
	"image/x-icon":             true,
	"image/vnd.microsoft.icon": true,
}

// dangerousExtensions are extensions that a browser may execute or
// render inline regardless of declared Content-Type. We force these
// to application/octet-stream and Content-Disposition: attachment.
var dangerousExtensions = map[string]bool{
	".html": true, ".htm": true, ".xhtml": true, ".shtml": true,
	".svg": true, ".svgz": true,
	".xml": true, ".xsl": true, ".xslt": true,
	".js": true, ".mjs": true, ".cjs": true, ".jsx": true,
	".wasm":  true,
	".swf":   true,
	".jar":   true,
	".class": true,
	".jnlp":  true,
	// Executable/script extensions on common platforms. Even though
	// the browser doesn't run them, a download that opens an OS
	// installer with one click is undesirable when it came from a
	// chat message.
	".exe": true, ".msi": true, ".bat": true, ".cmd": true,
	".com": true, ".scr": true, ".sh": true, ".ps1": true,
	".vbs": true, ".vbe": true, ".wsf": true, ".wsh": true,
	".dll": true, ".lnk": true, ".reg": true,
	// Apple/Linux executables.
	".app": true, ".pkg": true, ".dmg": true, ".deb": true, ".rpm": true,
	".elf": true, ".bin": true,
}

// PeekSize is how many bytes we pull off the front of an upload to
// classify it. 512 is the http.DetectContentType contract.
const PeekSize = 512

// SafeFile holds the post-validation upload metadata the caller hands
// to MinIO and Postgres. All fields are non-empty; SafeFile.Reader is
// the body reader the caller should send to MinIO (it's been "rewound"
// using a peek-buffer so the magic bytes are still part of the stream).
type SafeFile struct {
	// Reader is the upload body, with the peeked prefix re-prepended.
	// Pass this to minio.PutObject. The original io.Reader handed to
	// PrepareUpload must NOT be reused after this point.
	Reader io.Reader

	// SafeName is the sanitised filename — printable Unicode only,
	// no path separators or control chars, length-capped.
	SafeName string

	// ContentType is what the server should set on PutObject and what
	// downstream presigned-GETs will surface. Already coerced to
	// application/octet-stream when the file is in dangerousExtensions.
	ContentType string

	// ContentDisposition is the full header value (`attachment; filename=...`)
	// the server should set on PutObject so MinIO surfaces it on every
	// presigned GET.
	ContentDisposition string

	// Inline is true when ContentDisposition uses "inline" (image
	// preview) instead of "attachment". Useful for callers that want
	// to log how the file will be served.
	Inline bool

	// DetectedType is the raw http.DetectContentType output for the
	// peeked prefix. Useful for telemetry; not security-critical.
	DetectedType string
}

// Options tunes PrepareUpload behaviour. Pass a zero value for
// "use the conservative defaults".
type Options struct {
	// AllowedExtensions, when non-empty, restricts uploads to this
	// allowlist (case-insensitive, leading dot included e.g. ".pdf").
	// Use this on the admin-config-logo path, project-icon path, etc.
	// Leave empty for general-purpose chat/file uploads.
	AllowedExtensions []string

	// AllowInlineImages, when true, returns Content-Disposition: inline
	// for entries in inlineSafeTypes. Default (false) is to always
	// force attachment, which is the safest disposition.
	AllowInlineImages bool

	// MaxNameLen caps the safe-filename length. 0 → 200.
	MaxNameLen int
}

// PrepareUpload reads the leading bytes of body to classify it, then
// returns a SafeFile carrying the sanitised metadata and a Reader that
// has the peeked bytes restored so the caller can pipe straight to
// MinIO.
//
// Validation order:
//
//  1. Read up to PeekSize bytes (or the full body, whichever is less).
//  2. Empty bodies → ErrEmptyBody.
//  3. Sanitise the filename (printable Unicode, no separators).
//  4. If AllowedExtensions is set, reject names outside it.
//  5. Detect MIME from peeked bytes (http.DetectContentType).
//  6. Coerce dangerous extensions to application/octet-stream.
//  7. Build Content-Disposition: attachment by default; inline only
//     when AllowInlineImages is true AND DetectedType ∈ inlineSafeTypes
//     AND the extension matches the type.
func PrepareUpload(body io.Reader, suppliedName string, opts Options) (*SafeFile, error) {
	if body == nil {
		return nil, ErrEmptyBody
	}

	// Buffer the head so we can both peek for magic-byte and re-stream
	// to MinIO. bufio.Reader with the peek size hint is the cheapest
	// way to do this without copying the whole body.
	br := bufio.NewReaderSize(body, PeekSize)
	peek, err := br.Peek(PeekSize)
	if err != nil && err != io.EOF && err != bufio.ErrBufferFull {
		return nil, fmt.Errorf("read upload prefix: %w", err)
	}
	if len(peek) == 0 {
		return nil, ErrEmptyBody
	}

	safeName := SanitiseFileName(suppliedName, capOrDefault(opts.MaxNameLen, 200))
	ext := strings.ToLower(filepath.Ext(safeName))

	if len(opts.AllowedExtensions) > 0 {
		ok := false
		for _, allowed := range opts.AllowedExtensions {
			if strings.EqualFold(ext, allowed) {
				ok = true
				break
			}
		}
		if !ok {
			return nil, fmt.Errorf("%w: extension %q not allowed", ErrUnsupportedType, ext)
		}
	}

	detected := http.DetectContentType(peek)
	contentType := SafeContentType(detected, ext)

	inline := false
	disposition := buildAttachmentDisposition(safeName)
	if opts.AllowInlineImages && inlineSafeTypes[strippedMediaType(detected)] && imageExtMatches(detected, ext) {
		inline = true
		disposition = buildInlineDisposition(safeName)
	}

	return &SafeFile{
		Reader:             br,
		SafeName:           safeName,
		ContentType:        contentType,
		ContentDisposition: disposition,
		Inline:             inline,
		DetectedType:       detected,
	}, nil
}

// SafeContentType picks a safe Content-Type for storage/serving.
// If the extension is in dangerousExtensions, returns
// application/octet-stream regardless of detected type. Otherwise
// returns detected (which is what http.DetectContentType produced).
//
// Exposed so callers that already have validated bytes (e.g. internal
// import workers fetching from a trusted-but-rate-limited CDN) can
// use the same coercion logic without re-peeking.
func SafeContentType(detected, ext string) string {
	if dangerousExtensions[strings.ToLower(ext)] {
		return "application/octet-stream"
	}
	if detected == "" {
		return "application/octet-stream"
	}
	// http.DetectContentType returns "text/html; charset=utf-8" for HTML
	// and similar — for storage we keep the full string so the browser
	// gets the charset hint, but for security decisions we use the
	// stripped media type only. The stored value is fine to keep.
	if strippedMediaType(detected) == "text/html" {
		// Double-defence: an HTML body with a non-dangerous extension
		// (rare, but possible — e.g. .pdf containing HTML payload to
		// confuse some renderer). Refuse to serve it as text/html.
		return "application/octet-stream"
	}
	return detected
}

// BuildDisposition returns a `Content-Disposition` header value for
// the given filename. Always uses `attachment`. Use BuildInlineDisposition
// for the rare case of inline image rendering.
func BuildDisposition(filename string) string {
	return buildAttachmentDisposition(filename)
}

// BuildInlineDisposition returns `inline; filename="..."`. Use only
// for content types in inlineSafeTypes after extension cross-check.
func BuildInlineDisposition(filename string) string {
	return buildInlineDisposition(filename)
}

// SanitiseFileName produces a cross-platform-safe filename. Keeps
// printable Unicode (so "résumé.pdf" survives), strips path separators
// and control characters, and trims to maxLen runes (NOT bytes — this
// keeps multi-byte scripts intact).
//
// Returns "untitled" when the input would sanitise to empty.
func SanitiseFileName(in string, maxLen int) string {
	if maxLen <= 0 {
		maxLen = 200
	}
	if in == "" {
		return "untitled"
	}
	// Strip any path component the user/CDN may have included.
	in = filepath.Base(in)
	if in == "." || in == ".." || in == string(filepath.Separator) {
		return "untitled"
	}

	var b strings.Builder
	for _, r := range in {
		switch {
		case r == utf8.RuneError:
			continue
		case r < 32 || r == 0x7f:
			continue
		case r == '/' || r == '\\':
			continue
		case r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|':
			// Reserved on Windows; preferred to avoid for
			// cross-platform safety.
			continue
		case unicode.IsPrint(r):
			b.WriteRune(r)
		}
	}

	out := strings.TrimSpace(b.String())
	out = strings.Trim(out, ".") // no all-dot filenames
	if out == "" {
		return "untitled"
	}

	// Length cap by rune count, preserving extension when possible.
	if utf8.RuneCountInString(out) <= maxLen {
		return out
	}
	ext := filepath.Ext(out)
	stem := strings.TrimSuffix(out, ext)
	stemMax := maxLen - utf8.RuneCountInString(ext)
	if stemMax < 8 {
		// Pathological filename (ext alone > maxLen). Truncate the
		// whole string by rune count and call it good enough.
		return runeTruncate(out, maxLen)
	}
	return runeTruncate(stem, stemMax) + ext
}

// PeekAndIsZip is a convenience: peek the first 4 bytes of body, return
// (true, restored-reader) if it's a zip; otherwise return false and
// either an error or a restored reader (caller may want to stream the
// non-zip body to a placeholder location for diagnostics).
//
// The returned reader is always a fresh io.Reader that includes the
// peeked bytes — pass it on as the request body.
func PeekAndIsZip(body io.Reader) (bool, io.Reader, error) {
	if body == nil {
		return false, nil, ErrEmptyBody
	}
	br := bufio.NewReaderSize(body, 4)
	peek, err := br.Peek(4)
	if err != nil && err != io.EOF {
		return false, br, fmt.Errorf("peek upload prefix: %w", err)
	}
	if len(peek) < 4 {
		return false, br, ErrEmptyBody
	}
	// Accept all three PKZIP magic prefixes the import pipelines use.
	return isZipMagic(peek), br, nil
}

// ─── internals ─────────────────────────────────────────────────────

// isZipMagic mirrors zipsafe.IsZipMagic without the import cycle that
// would arise if uploadsafe depended on zipsafe. The byte sequences are
// publicly defined (PKZIP spec) so duplicating them is harmless.
func isZipMagic(prefix []byte) bool {
	if len(prefix) < 4 {
		return false
	}
	head := prefix[:4]
	return bytes.Equal(head, []byte{'P', 'K', 0x03, 0x04}) ||
		bytes.Equal(head, []byte{'P', 'K', 0x05, 0x06}) ||
		bytes.Equal(head, []byte{'P', 'K', 0x07, 0x08})
}

// strippedMediaType returns "image/png" from "image/png; charset=binary".
func strippedMediaType(s string) string {
	if i := strings.IndexByte(s, ';'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// imageExtMatches checks the extension matches the detected MIME so a
// rogue caller can't get inline disposition by claiming an image MIME
// for a .html body.
func imageExtMatches(detected, ext string) bool {
	switch strippedMediaType(detected) {
	case "image/png":
		return ext == ".png"
	case "image/jpeg":
		return ext == ".jpg" || ext == ".jpeg"
	case "image/gif":
		return ext == ".gif"
	case "image/webp":
		return ext == ".webp"
	case "image/bmp":
		return ext == ".bmp"
	case "image/x-icon", "image/vnd.microsoft.icon":
		return ext == ".ico"
	}
	return false
}

// buildAttachmentDisposition returns a properly-quoted disposition
// header value with both legacy `filename=` and RFC 5987 `filename*=`
// for non-ASCII names.
func buildAttachmentDisposition(name string) string {
	return "attachment; " + filenameDispositionPart(name)
}

func buildInlineDisposition(name string) string {
	return "inline; " + filenameDispositionPart(name)
}

// filenameDispositionPart produces the `filename=...; filename*=UTF-8”...`
// segment used in Content-Disposition. Always quotes the legacy form
// and percent-encodes the RFC-5987 form so non-ASCII names work in
// modern browsers.
func filenameDispositionPart(name string) string {
	asciiSafe := keepASCII(name)
	if asciiSafe == "" {
		asciiSafe = "download"
	}
	rfc5987 := pctEncodeRFC5987(name)
	return fmt.Sprintf(`filename="%s"; filename*=UTF-8''%s`, escapeQuotes(asciiSafe), rfc5987)
}

func keepASCII(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 32 && r < 127 && r != '"' && r != '\\' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func escapeQuotes(s string) string {
	return strings.ReplaceAll(s, `"`, `\"`)
}

// pctEncodeRFC5987 percent-encodes per RFC 5987 attr-char rules.
func pctEncodeRFC5987(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
			b.WriteByte(c)
		case c == '!' || c == '#' || c == '$' || c == '&' || c == '+' || c == '-' ||
			c == '.' || c == '^' || c == '_' || c == '`' || c == '|' || c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func runeTruncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count >= n {
			return s[:i]
		}
		count++
	}
	return s
}

func capOrDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}
