package zipsafe

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestIsZipMagic(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"empty", []byte{}, false},
		{"too short", []byte{'P', 'K'}, false},
		{"local file header", []byte{'P', 'K', 0x03, 0x04, 0x14}, true},
		{"eocd", []byte{'P', 'K', 0x05, 0x06}, true},
		{"span", []byte{'P', 'K', 0x07, 0x08}, true},
		{"random bytes", []byte{'a', 'b', 'c', 'd'}, false},
		{"html start", []byte{'<', '!', 'D', 'O'}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsZipMagic(tc.in); got != tc.want {
				t.Errorf("IsZipMagic(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSafePath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"", false},
		{"users.json", true},
		{"general/2024-01-15.json", true},
		{"/abs.json", false},
		{`\abs.json`, false},
		{"../escape.json", false},
		{"a/../b", false},
		{"a/..", false},
		{"C:/win.json", false},
		{"foo/bar.json", true},
		{"emoji 📦/file.json", true},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if got := SafePath(tc.path); got != tc.want {
				t.Errorf("SafePath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// makeBomb builds an in-memory zip whose declared sizes simulate a
// zip-bomb (huge UncompressedSize64 with a tiny CompressedSize64).
// We can't actually craft real DEFLATE bytes for that ratio without a
// real bomb file; instead we use Store-method entries with a forged
// header by building the zip manually-enough to satisfy archive/zip
// for metadata reading.
//
// For the purposes of ValidateZip we just need a zip whose .File slice
// has the desired metadata; archive/zip exposes those fields directly.
func TestValidateZip_TooManyEntries(t *testing.T) {
	zr := buildZip(t, 100, 1024, 1024) // 100 entries
	limits := DefaultLimits()
	limits.MaxEntries = 50
	err := ValidateZip(zr, limits)
	if !errors.Is(err, ErrTooManyEntries) {
		t.Fatalf("want ErrTooManyEntries, got %v", err)
	}
}

func TestValidateZip_EntryTooLarge(t *testing.T) {
	zr := buildZip(t, 1, 100, 100)
	// Force a per-entry cap of 1 byte to trigger.
	limits := DefaultLimits()
	limits.MaxPerEntryUncompressed = 1
	err := ValidateZip(zr, limits)
	if !errors.Is(err, ErrEntryTooLarge) {
		t.Fatalf("want ErrEntryTooLarge, got %v", err)
	}
}

func TestValidateZip_OK(t *testing.T) {
	zr := buildZip(t, 5, 1024, 1024)
	if err := ValidateZip(zr, DefaultLimits()); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}

func TestValidateZip_UnsafePath(t *testing.T) {
	// Build a zip and inject an unsafe path manually by editing the
	// resulting File entry name. ValidateZip rejects on the metadata
	// pass before any decompression.
	zr := buildZip(t, 1, 100, 100)
	zr.File[0].Name = "../escape"
	if err := ValidateZip(zr, DefaultLimits()); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("want ErrUnsafePath, got %v", err)
	}
}

func TestLimitReader(t *testing.T) {
	src := strings.NewReader("hello world")
	wrapped := LimitReader(io.NopCloser(src), 5)
	defer wrapped.Close()
	buf := make([]byte, 100)
	n, err := wrapped.Read(buf)
	if err != nil && !errors.Is(err, ErrDecompressedExceeded) {
		t.Fatalf("unexpected error: %v", err)
	}
	if n > 5 {
		t.Fatalf("read %d bytes; cap was 5", n)
	}
}

// buildZip writes an n-entry in-memory zip and returns a zip.Reader.
// each entry is "data" repeated `size` times to produce a deterministic
// uncompressed size.
func buildZip(t *testing.T, n, size, comp int) *zip.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i < n; i++ {
		w, err := zw.Create(makeName(i))
		if err != nil {
			t.Fatalf("create entry %d: %v", i, err)
		}
		if _, err := w.Write(bytes.Repeat([]byte("a"), size)); err != nil {
			t.Fatalf("write entry %d: %v", i, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	rdr := bytes.NewReader(buf.Bytes())
	zr, err := zip.NewReader(rdr, int64(buf.Len()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = comp
	return zr
}

func makeName(i int) string {
	return "f" + string(rune('a'+i%26)) + ".txt"
}
