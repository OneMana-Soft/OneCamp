package helpers

// Naming and typing for uploaded files, in one place.
//
// These two functions existed three and three times respectively — byte-identical copies
// in business/Import and business/SlackImport, plus a third, DIFFERENT and unreachable
// variant in business/User. The duplication was deliberate and its reason was recorded at
// the copy: "we keep a lightweight copy here to avoid an import cycle". That reason is
// real between two business packages and evaporates in helpers, which imports nothing
// from the domain.
//
// Worth consolidating rather than leaving alone because of what these decide. One of them
// says which characters may appear in an object key; the other says what Content-Type a
// browser will be handed for a file someone uploaded. Copies of a rule like that do not
// stay equal — a fix applied to one is a divergence from the other, and the divergence is
// invisible until the two paths disagree about the same file. The dead third variant in
// business/User already disagreed: it allowed only alphanumerics plus -_. and would have
// mangled any name with a space or a non-Latin character.

import (
	"mime"
	"path/filepath"
	"strings"
)

const (
	// maxUploadFileNameLen bounds a stored name. Object stores accept far longer keys;
	// the cap is about what stays readable in a UI and predictable in a URL.
	maxUploadFileNameLen = 128

	// fallbackUploadFileName is used when a name sanitises down to nothing, so an
	// object key is never empty and never collides on emptiness.
	fallbackUploadFileName = "untitled"

	// defaultUploadContentType is the honest answer when the extension says nothing.
	// Deliberately not text/plain or an HTML-adjacent type: a browser handed an
	// unrecognised file should download it, not try to render it.
	defaultUploadContentType = "application/octet-stream"
)

// SanitizeUploadFileName returns a filename safe to use in an object key.
//
// Strips control characters (including DEL) and both path separators, so a name can
// never climb out of its prefix or smuggle a directory boundary. Everything else is
// kept — spaces, punctuation, and non-Latin scripts are legitimate in a filename, and a
// stricter allowlist silently renames other people's files.
//
// Preserved exactly as the two import paths had it, so consolidating changes no object
// key any existing import would have produced.
func SanitizeUploadFileName(s string) string {
	if s == "" {
		return fallbackUploadFileName
	}
	var b strings.Builder
	for _, r := range s {
		if r >= 32 && r != '/' && r != '\\' && r != 0x7f {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > maxUploadFileNameLen {
		out = out[:maxUploadFileNameLen]
	}
	// A name made entirely of stripped characters would otherwise return "".
	if out == "" {
		return fallbackUploadFileName
	}
	return out
}

// GuessContentTypeFromName resolves a Content-Type from a filename's extension, for
// when the source did not send one — a CDN that omits the header, or a file read out of
// an export archive.
//
// Extension-based rather than content-sniffing on purpose: sniffing an untrusted upload
// can classify it as something a browser will execute, which turns a file store into an
// XSS surface. An extension is the uploader's own claim and is bounded by the mime
// registry, and anything unrecognised is served as a download.
func GuessContentTypeFromName(name string) string {
	if ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); ct != "" {
		return ct
	}
	return defaultUploadContentType
}
