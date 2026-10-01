package helpers

import (
	"strings"
	"testing"
)

// The properties that make an object key safe. These were previously asserted nowhere:
// the rule lived in two identical copies and one divergent dead one, and none had a test.
func TestSanitizeUploadFileNameStripsWhatCanBreakAKey(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"path separators cannot survive", "../../etc/passwd", "....etcpasswd"},
		{"windows separators too", `..\..\windows\system32`, "....windowssystem32"},
		{"control characters go", "re\x00port\x1f.pdf", "report.pdf"},
		{"DEL goes", "notes\x7f.txt", "notes.txt"},
		{"ordinary names are untouched", "Q3 Report (final) v2.pdf", "Q3 Report (final) v2.pdf"},
		{"non-latin scripts are kept", "会議メモ.txt", "会議メモ.txt"},
		{"empty becomes a placeholder", "", fallbackUploadFileName},
		{"all-stripped becomes a placeholder", "///\\\\\x01", fallbackUploadFileName},
	}
	for _, c := range cases {
		if got := SanitizeUploadFileName(c.in); got != c.want {
			t.Errorf("%s: SanitizeUploadFileName(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}

	// The invariants that actually matter, over every case: a key can never contain a
	// separator or a control character, and is never empty.
	for _, c := range cases {
		got := SanitizeUploadFileName(c.in)
		if got == "" {
			t.Errorf("%q produced an empty key", c.in)
		}
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("%q produced %q, which still contains a path separator", c.in, got)
		}
		for _, r := range got {
			if r < 32 || r == 0x7f {
				t.Errorf("%q produced %q, which still contains a control character", c.in, got)
			}
		}
	}
}

// Long names are bounded, and the bound must not cut a multi-byte character in half —
// a truncated rune would make the key invalid UTF-8.
func TestSanitizeUploadFileNameBoundsLength(t *testing.T) {
	long := strings.Repeat("a", 500) + ".pdf"
	got := SanitizeUploadFileName(long)
	if len(got) > maxUploadFileNameLen {
		t.Errorf("length %d exceeds the %d cap", len(got), maxUploadFileNameLen)
	}
	// Byte-slicing a run of multi-byte runes is where an invalid key would come from.
	multi := strings.Repeat("会", 200)
	if out := SanitizeUploadFileName(multi); len(out) > maxUploadFileNameLen {
		t.Errorf("multibyte length %d exceeds the %d cap", len(out), maxUploadFileNameLen)
	}
}

// Content type comes from the extension, and anything unknown must be a type a browser
// downloads rather than renders — sniffing or guessing text/html here would turn the
// file store into an XSS surface.
func TestGuessContentTypeFromName(t *testing.T) {
	for _, c := range []struct{ in, wantPrefix string }{
		{"photo.png", "image/png"},
		{"PHOTO.PNG", "image/png"}, // extension match is case-insensitive
		{"doc.pdf", "application/pdf"},
		{"notes.txt", "text/plain"},
	} {
		if got := GuessContentTypeFromName(c.in); !strings.HasPrefix(got, c.wantPrefix) {
			t.Errorf("GuessContentTypeFromName(%q) = %q, want prefix %q", c.in, got, c.wantPrefix)
		}
	}
	for _, unknown := range []string{"payload", "archive.weirdext", "", "noext."} {
		if got := GuessContentTypeFromName(unknown); got != defaultUploadContentType {
			t.Errorf("GuessContentTypeFromName(%q) = %q, want %q — an unrecognised upload must be "+
				"served as a download, never as something a browser will render",
				unknown, got, defaultUploadContentType)
		}
	}
}
