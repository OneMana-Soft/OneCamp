package zipsafe

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"runtime"
	"strings"
	"testing"
)

// zipOf is a zip of n empty stored entries, the i-th named name(i).
func zipOf(t *testing.T, n int, name func(i int) string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i < n; i++ {
		if _, err := zw.CreateHeader(&zip.FileHeader{Name: name(i), Method: zip.Store}); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func short(i int) string { return string(rune('a' + i%26)) }

// A zip within its limits opens as archive/zip opens it: a small one, one
// past 65,535 entries (its zip64 end record), and one with bytes before it
// (a self-extracting zip, read from its base offset).
func TestOpenReadsAZipWithinItsLimits(t *testing.T) {
	small := zipOf(t, 3, short)
	big := zipOf(t, 70_000, short)
	for _, c := range []struct {
		name string
		zip  []byte
		want int
	}{
		{"three files", small, 3},
		{"seventy thousand, with zip64", big, 70_000},
		{"bytes before it", append(bytes.Repeat([]byte("MZ stub "), 128), small...), 3},
	} {
		zr, err := Open(bytes.NewReader(c.zip), int64(len(c.zip)), Limits{MaxEntries: 100_000, MaxDirectoryBytes: 64 << 20})
		if err != nil || len(zr.File) != c.want {
			t.Fatalf("%s: %v, %v", c.name, zr, err)
		}
	}
}

// A zip past its count is refused before archive/zip reads its directory:
// what refusing it costs is a few kilobytes, where archive/zip spends tens of
// megabytes on the same zip before anyone can count its files.
func TestOpenRefusesTooManyEntriesBeforeReadingThem(t *testing.T) {
	z := zipOf(t, 120_000, short)
	allocated := func(open func()) uint64 {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		open()
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	var err error
	guarded := allocated(func() {
		_, err = Open(bytes.NewReader(z), int64(len(z)), Limits{MaxEntries: 50_000, MaxDirectoryBytes: 64 << 20})
	})
	if !errors.Is(err, ErrTooManyEntries) {
		t.Fatalf("120,000 entries past 50,000: %v", err)
	}
	unguarded := allocated(func() {
		if zr, err := zip.NewReader(bytes.NewReader(z), int64(len(z))); err != nil || len(zr.File) != 120_000 {
			t.Fatalf("archive/zip read %v, %v", zr, err)
		}
	})
	if guarded > 1<<20 || unguarded < 16<<20 {
		t.Fatalf("refusing allocated %d bytes, archive/zip %d", guarded, unguarded)
	}
}

// The directory's size counts too: few entries, with long names.
func TestOpenRefusesADirectoryPastItsSize(t *testing.T) {
	z := zipOf(t, 100, func(i int) string { return strings.Repeat("n", 300) + short(i) })
	if _, err := Open(bytes.NewReader(z), int64(len(z)), Limits{MaxEntries: 1000, MaxDirectoryBytes: 10_000}); !errors.Is(err, ErrDirectoryTooLarge) {
		t.Fatalf("a 35 KB directory past 10 KB: %v", err)
	}
}

// archive/zip reads records past the count a zip claims, and compares only
// its low 16 bits: an end record claiming one file before 65,537 is read
// whole. Open counts the records, not the claim.
func TestOpenCountsTheRecordsAZipHidesPastItsClaim(t *testing.T) {
	z := zipOf(t, 65_537, short)
	// The writer ended it with a zip64 end record, its locator and an end
	// record. Keep the directory, and end it with an end record that claims
	// one file.
	loc := len(z) - directoryEndLen - directory64LocLen
	end64 := int(binary.LittleEndian.Uint64(z[loc+8 : loc+16]))
	dirSize := binary.LittleEndian.Uint64(z[end64+40 : end64+48])
	dirOffset := binary.LittleEndian.Uint64(z[end64+48 : end64+56])
	lying := append([]byte(nil), z[:dirOffset+dirSize]...)
	end := make([]byte, directoryEndLen)
	binary.LittleEndian.PutUint32(end[0:4], 0x06054b50)
	binary.LittleEndian.PutUint16(end[8:10], 1)
	binary.LittleEndian.PutUint16(end[10:12], 1)
	binary.LittleEndian.PutUint32(end[12:16], uint32(dirSize))
	binary.LittleEndian.PutUint32(end[16:20], uint32(dirOffset))
	lying = append(lying, end...)

	zr, err := zip.NewReader(bytes.NewReader(lying), int64(len(lying)))
	if err != nil || len(zr.File) != 65_537 {
		t.Fatalf("archive/zip read the lying zip as %v, %v: the case no longer stands", zr, err)
	}
	if _, err := Open(bytes.NewReader(lying), int64(len(lying)), Limits{MaxEntries: 50_000}); !errors.Is(err, ErrTooManyEntries) {
		t.Fatalf("a zip claiming one file and holding 65,537: %v", err)
	}
}

// What isn't a zip isn't opened.
func TestOpenRefusesWhatIsNotAZip(t *testing.T) {
	for _, b := range [][]byte{nil, []byte("PK"), bytes.Repeat([]byte("not a zip "), 200)} {
		if _, err := Open(bytes.NewReader(b), int64(len(b)), Limits{MaxEntries: 10}); err == nil {
			t.Fatalf("%q opened", b)
		}
	}
}
