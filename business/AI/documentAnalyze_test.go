package business

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

// buildDocx builds a minimal valid .docx (zip with word/document.xml) whose
// body contains the given paragraphs, for testing extractDocx.
func buildDocx(t *testing.T, paras ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	var body strings.Builder
	body.WriteString(`<?xml version="1.0"?><w:document xmlns:w="x"><w:body>`)
	for _, p := range paras {
		body.WriteString("<w:p><w:r><w:t>")
		body.WriteString(p)
		body.WriteString("</w:t></w:r></w:p>")
	}
	body.WriteString(`</w:body></w:document>`)
	if _, err := w.Write([]byte(body.String())); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestExtractDocumentText_Text(t *testing.T) {
	in := []byte("# Title\n\nHello, world.\nThis is a plain text doc.")
	out, err := extractDocumentText(in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(out, "Hello, world.") || !strings.Contains(out, "plain text doc") {
		t.Fatalf("text not extracted: %q", out)
	}
}

func TestExtractDocumentText_Docx(t *testing.T) {
	data := buildDocx(t, "First paragraph.", "Second paragraph.")
	out, err := extractDocumentText(data)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(out, "First paragraph.") || !strings.Contains(out, "Second paragraph.") {
		t.Fatalf("docx text not extracted: %q", out)
	}
	// Paragraph boundary should produce a newline.
	if !strings.Contains(out, "First paragraph.\n") {
		t.Fatalf("docx paragraph break missing: %q", out)
	}
}

func TestExtractDocumentText_InvalidPDF(t *testing.T) {
	// A file that claims to be a PDF (magic bytes) but isn't valid must degrade
	// to a friendly error via the parser + panic-guard, never crash.
	in := []byte("%PDF-1.7\nnot a real pdf body...")
	_, err := extractDocumentText(in)
	if err == nil {
		t.Fatalf("expected an error for a malformed PDF")
	}
}

func TestExtractDocumentText_Binary(t *testing.T) {
	in := []byte{0x00, 0x01, 0x02, 0xff, 0xfe, 0x03, 0x04, 0x00, 0x10, 0x80}
	if _, err := extractDocumentText(in); err == nil {
		t.Fatalf("expected unsupported/binary error")
	}
}

func TestExtractDocumentText_Empty(t *testing.T) {
	if _, err := extractDocumentText(nil); err != errEmptyDoc {
		t.Fatalf("want errEmptyDoc, got %v", err)
	}
}

func TestLooksTextual(t *testing.T) {
	if !looksTextual([]byte("plain ascii text\nwith newlines\tand tabs")) {
		t.Fatalf("ascii text should be textual")
	}
	if !looksTextual([]byte("unicode: café résumé — ✅")) {
		t.Fatalf("valid utf-8 should be textual")
	}
	// Mostly control bytes → not textual.
	binary := make([]byte, 100)
	for i := range binary {
		binary[i] = byte(i % 7) // lots of low control bytes
	}
	if looksTextual(binary) {
		t.Fatalf("binary should not be textual")
	}
}
