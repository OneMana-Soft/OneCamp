package business

import (
	"archive/zip"
	"bytes"
	"errors"
	"runtime"
	"strings"
	"testing"
)

// manyEntries is a zip of n empty files with one-letter names: the most
// records for its size.
func manyEntries(t *testing.T, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i < n; i++ {
		if _, err := zw.CreateHeader(&zip.FileHeader{Name: string(rune('a' + i%26)), Method: zip.Store}); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// An import's zip is opened by the zip-open rule (zipsafe.Open): one with more
// than maxZipEntries files is refused before archive/zip reads its list of
// them, with the import's own answer. That holds for the zip itself, a zip at
// its top (a Notion export's part), and a Word file in it, each read with
// what its own reading takes.
func TestAZipPastItsFileCountIsRefusedBeforeItsListIsRead(t *testing.T) {
	many := manyEntries(t, maxZipEntries+1)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := OpenZip(bytes.NewReader(many), int64(len(many)))
	runtime.ReadMemStats(&after)
	if spent := after.TotalAlloc - before.TotalAlloc; spent > 1<<20 {
		t.Fatalf("refusing it allocated %d bytes: its list was read (%v)", spent, err)
	}
	var refused readError
	if !errors.As(err, &refused) || !strings.Contains(refused.msg, "The zip holds more files than an import reads (50000 at most).") {
		t.Fatalf("a zip of %d files: %v", maxZipEntries+1, err)
	}

	var outer bytes.Buffer
	zw := zip.NewWriter(&outer)
	w, err := zw.Create("Export-1234-Part-1.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(many); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenZip(bytes.NewReader(outer.Bytes()), int64(outer.Len())); !errors.As(err, &refused) ||
		!strings.Contains(refused.msg, "Export-1234-Part-1.zip, inside the zip, holds more files than an import reads") {
		t.Fatalf("a part of %d files: %v", maxZipEntries+1, err)
	}

	if _, err := openPackage(bytes.NewReader(many), int64(len(many)), nil); !errors.As(err, &refused) ||
		!strings.Contains(refused.msg, "The file holds more files than an import reads") {
		t.Fatalf("a Word file of %d parts: %v", maxZipEntries+1, err)
	}

	// At the count, it opens.
	at := manyEntries(t, maxZipEntries)
	if _, err := OpenZip(bytes.NewReader(at), int64(len(at))); err != nil {
		t.Fatalf("a zip of %d files: %v", maxZipEntries, err)
	}
}
