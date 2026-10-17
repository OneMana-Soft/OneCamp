package business

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"io"
	"math"
	"testing"
)

// Every zip an import reads, a Word file included, is unpacked by one rule
// (unpackBudget): within the import's budget, and never past the size each
// file declares. A zip64 entry may declare up to 2⁶⁴ bytes: a Word file's
// image declaring more than 2⁶³ read as -1, which passed the upload limit,
// and its deflate stream went to storage unbounded (GENERIC-BE, 15 Oct 2026:
// 4 MB through a 1 MB limit).

// zipWith is a zip of the files given, plus one entry whose header declares
// declared bytes while its deflate stream holds real bytes.
func zipWith(t *testing.T, files map[string]string, forged string, declared uint64, real int) []byte {
	t.Helper()
	var comp bytes.Buffer
	fw, _ := flate.NewWriter(&comp, flate.BestCompression)
	if _, err := fw.Write(make([]byte, real)); err != nil {
		t.Fatal(err)
	}
	fw.Close()
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, body)
	}
	w, err := zw.CreateRaw(&zip.FileHeader{Name: forged, Method: zip.Deflate, CompressedSize64: uint64(comp.Len()), UncompressedSize64: declared})
	if err != nil {
		t.Fatal(err)
	}
	w.Write(comp.Bytes())
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

const unpackWordBody = `<?xml version="1.0" encoding="UTF-8"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>Launch notes</w:t></w:r></w:p></w:body></w:document>`

func wordWith(t *testing.T, declared uint64, real int) []byte {
	return zipWith(t, map[string]string{"word/document.xml": unpackWordBody}, "word/media/image1.png", declared, real)
}

func TestAWordImageClaimingMoreThanCanBeUnpackedIsRefusedUnread(t *testing.T) {
	const limit = 1 << 20
	for _, declared := range []uint64{math.MaxUint64, 1 << 63, maxZipUnpacked + 1} {
		data := wordWith(t, declared, 4<<20)
		pg, err := docxPage("notes.docx", bytes.NewReader(data), int64(len(data)), nil)
		if err != nil {
			t.Fatal(err)
		}
		if r, size, err := pg.Assets.Open("word/media/image1.png"); err == nil {
			n, _ := io.Copy(io.Discard, r)
			r.Close()
			t.Fatalf("declared %d: opened with size %d, %d bytes read", declared, size, n)
		}
		if r, size, _, err := (&docMaker{}).openImage("word", "media/image1.png", pg.Assets, limit); err == nil {
			r.Close()
			t.Fatalf("declared %d: an image past the %d-byte limit was let through (size %d)", declared, limit, size)
		}
	}
}

// A file is read no further than the size it declares, whatever its deflate
// stream holds.
func TestAWordImageIsReadNoFurtherThanItsDeclaredSize(t *testing.T) {
	data := wordWith(t, 100, 4<<20)
	pg, err := docxPage("notes.docx", bytes.NewReader(data), int64(len(data)), nil)
	if err != nil {
		t.Fatal(err)
	}
	r, size, err := pg.Assets.Open("word/media/image1.png")
	if err != nil || size != 100 {
		t.Fatalf("a 100-byte image: size %d, %v", size, err)
	}
	n, _ := io.Copy(io.Discard, r)
	r.Close()
	if n > 100 {
		t.Fatalf("read %d bytes of an image that declares 100", n)
	}
}

// An import's zip refuses the same forged size.
func TestAZipFileClaimingMoreThanCanBeUnpackedIsRefusedUnread(t *testing.T) {
	data := zipWith(t, map[string]string{"Notes.md": "# Notes"}, "img/big.png", math.MaxUint64, 4<<20)
	z, err := OpenZip(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	if r, size, err := z.Assets().Open("img/big.png"); err == nil {
		r.Close()
		t.Fatalf("a file claiming 2⁶⁴ bytes opened with size %d", size)
	}
}

// A Word file in an import's zip unpacks within the zip's budget: what the
// zip has left is all its images may take.
func TestAWordFileInAZipUnpacksWithinTheZipsBudget(t *testing.T) {
	data := wordWith(t, 4096, 4096)
	budget := &unpackBudget{left: 1 << 10}
	pg, err := docxPage("Notes/notes.docx", bytes.NewReader(data), int64(len(data)), budget)
	if err != nil {
		t.Fatal(err)
	}
	if r, _, err := pg.Assets.Open("Notes/word/media/image1.png"); err == nil {
		r.Close()
		t.Fatal("a 4 KB image opened with 1 KB of the zip's budget left")
	}
}
