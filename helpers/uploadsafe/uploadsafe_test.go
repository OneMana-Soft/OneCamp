package uploadsafe

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestPrepareUpload_RejectsEmpty(t *testing.T) {
	if _, err := PrepareUpload(bytes.NewReader(nil), "x.pdf", Options{}); !errors.Is(err, ErrEmptyBody) {
		t.Fatalf("want ErrEmptyBody, got %v", err)
	}
}

func TestPrepareUpload_DangerousExtensionCoerced(t *testing.T) {
	body := bytes.NewReader([]byte("<html></html>"))
	sf, err := PrepareUpload(body, "evil.html", Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if sf.ContentType != "application/octet-stream" {
		t.Errorf("html should be octet-stream; got %q", sf.ContentType)
	}
	if !strings.HasPrefix(sf.ContentDisposition, "attachment;") {
		t.Errorf("disposition should start attachment; got %q", sf.ContentDisposition)
	}
}

func TestPrepareUpload_SVGCoerced(t *testing.T) {
	body := bytes.NewReader([]byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`))
	sf, err := PrepareUpload(body, "image.svg", Options{AllowInlineImages: true})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// SVG is XSS-prone — even with AllowInlineImages we coerce to octet-stream.
	if sf.ContentType != "application/octet-stream" {
		t.Errorf("svg should be octet-stream; got %q", sf.ContentType)
	}
	if sf.Inline {
		t.Errorf("svg should not be inline")
	}
}

func TestPrepareUpload_PNGAllowedInline(t *testing.T) {
	// Real PNG signature.
	pngHead := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	body := bytes.NewReader(append(pngHead, bytes.Repeat([]byte{0}, 600)...))
	sf, err := PrepareUpload(body, "ok.png", Options{AllowInlineImages: true})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !sf.Inline {
		t.Errorf("png should be inline")
	}
	if !strings.HasPrefix(sf.ContentType, "image/png") {
		t.Errorf("png should be image/png; got %q", sf.ContentType)
	}
}

func TestPrepareUpload_PNGNotAllowedInline(t *testing.T) {
	pngHead := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	body := bytes.NewReader(append(pngHead, bytes.Repeat([]byte{0}, 600)...))
	sf, err := PrepareUpload(body, "ok.png", Options{}) // AllowInlineImages false
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if sf.Inline {
		t.Errorf("png should be attachment when AllowInlineImages is false")
	}
}

func TestPrepareUpload_HTMLBytesWithSafeExtension(t *testing.T) {
	// A .pdf-named file with HTML body — defence-in-depth coercion
	// must catch this.
	body := bytes.NewReader([]byte("<!DOCTYPE html><html><body>x</body></html>"))
	sf, err := PrepareUpload(body, "looks-like.pdf", Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// The detected type is text/html → coerced.
	if sf.ContentType != "application/octet-stream" {
		t.Errorf("html bytes with .pdf should be octet-stream; got %q", sf.ContentType)
	}
}

func TestPrepareUpload_AllowedExtensionsEnforced(t *testing.T) {
	body := bytes.NewReader([]byte("PDF-CONTENT"))
	_, err := PrepareUpload(body, "file.exe", Options{
		AllowedExtensions: []string{".pdf", ".doc"},
	})
	if !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("want ErrUnsupportedType, got %v", err)
	}
}

func TestPrepareUpload_BodyReplayedFully(t *testing.T) {
	const payload = "hello world this is the upload body"
	body := strings.NewReader(payload)
	sf, err := PrepareUpload(body, "x.txt", Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	got, err := io.ReadAll(sf.Reader)
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	if string(got) != payload {
		t.Errorf("body replay mismatch: got %q want %q", got, payload)
	}
}

func TestSanitiseFileName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"normal.pdf", "normal.pdf"},
		{"résumé.pdf", "résumé.pdf"},
		{"../etc/passwd", "passwd"},
		{"my:file*.txt", "myfile.txt"},
		{"\x00\x01evil.bin", "evil.bin"},
		{"", "untitled"},
		{".", "untitled"},
		{"path/to/file.txt", "file.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := SanitiseFileName(tc.in, 200)
			if got != tc.want {
				t.Errorf("SanitiseFileName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPeekAndIsZip(t *testing.T) {
	zipBytes := append([]byte{'P', 'K', 0x03, 0x04}, []byte("rest")...)
	is, body, err := PeekAndIsZip(bytes.NewReader(zipBytes))
	if err != nil || !is {
		t.Fatalf("zip should be detected: is=%v err=%v", is, err)
	}
	got, _ := io.ReadAll(body)
	if !bytes.Equal(got, zipBytes) {
		t.Errorf("body not replayed: got %v want %v", got, zipBytes)
	}

	is, _, _ = PeekAndIsZip(bytes.NewReader([]byte("<!DOCTYPE")))
	if is {
		t.Errorf("html should not be detected as zip")
	}
}

// A stranger's file (a public form's) that a browser could run or render is
// refused, not stored defused: an executable or script by its name, and HTML
// by its bytes whatever its name. What a member uploads is still kept, as
// octet-stream and an attachment. Images and documents pass either way.
func TestPrepareUpload_RefuseRunnableForStrangers(t *testing.T) {
	refused := []struct{ name, body string }{
		{"setup.exe", "MZ\x90\x00\x03"},
		{"run.sh", "#!/bin/sh\necho hi\n"},
		{"logo.svg", `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`},
		{"page.html", "<html><body>hi</body></html>"},
		{"brief.pdf", "<!DOCTYPE html><html><script>alert(1)</script></html>"},
	}
	for _, c := range refused {
		if _, err := PrepareUpload(strings.NewReader(c.body), c.name, Options{RefuseRunnable: true}); !errors.Is(err, ErrUnsupportedType) {
			t.Errorf("%s from a stranger: %v, want ErrUnsupportedType", c.name, err)
		}
		if _, err := PrepareUpload(strings.NewReader(c.body), c.name, Options{}); err != nil {
			t.Errorf("%s from a member was refused: %v", c.name, err)
		}
	}
	kept := []struct{ name, body string }{
		{"shot.png", "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"},
		{"brief.pdf", "%PDF-1.7\n1 0 obj"},
		{"notes.txt", "steps to reproduce"},
		{"data.csv", "a,b\n1,2\n"},
	}
	for _, c := range kept {
		if _, err := PrepareUpload(strings.NewReader(c.body), c.name, Options{RefuseRunnable: true}); err != nil {
			t.Errorf("%s from a stranger was refused: %v", c.name, err)
		}
	}
}
