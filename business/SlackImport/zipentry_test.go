package business

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
)

// The single-request entry read, tested without MinIO.
//
// openZipEntryDirect fetches an entry's compressed bytes in ONE ranged request instead of
// letting archive/zip pull them through ReadAt in ~32 KB pieces. On a real Slack export that
// is the difference between thousands of HTTP requests and a handful, because daily message
// files are both the most numerous and the largest entries in the archive.
//
// It sat unwired for a long time on the grounds that switching the read path deserved a real
// export end to end. That was the wrong conclusion. What can actually break here is
// arithmetic — an entry's data offset, its compressed length, the inclusive end of a range,
// and whether the decompressor and the underlying body both get closed. None of that is
// about MinIO. Behind the entryRangeFetcher interface these tests serve ranges out of a real
// in-memory archive and compare against what archive/zip itself produces, byte for byte.
//
// What this does NOT cover, stated so the gap is not mistaken for coverage: MinIO's own
// range semantics. Those are exercised on every import today by minioReaderAt.ReadAt, which
// is how the archive's central directory is read in the first place — if ranged GETs were
// broken, no import would parse at all.

// memRanger serves byte ranges from an in-memory archive and records what was asked for, so
// a test can assert the fast path requests exactly the entry's bytes and nothing more.
type memRanger struct {
	data    []byte
	calls   []memRange
	handles []*trackedCloser
}

type memRange struct{ off, length int64 }

func (m *memRanger) fetchRange(_ context.Context, off, length int64) (io.ReadCloser, error) {
	m.calls = append(m.calls, memRange{off: off, length: length})
	if length <= 0 {
		return nil, fmt.Errorf("non-positive range length %d", length)
	}
	if off < 0 || off+length > int64(len(m.data)) {
		return nil, fmt.Errorf("range [%d,%d) outside object of size %d", off, off+length, len(m.data))
	}
	h := &trackedCloser{Reader: bytes.NewReader(m.data[off : off+length])}
	m.handles = append(m.handles, h)
	return h, nil
}

// trackedCloser records whether the body was closed, so the composite closer's promise to
// close both the decompressor and the underlying object can be checked.
type trackedCloser struct {
	io.Reader
	closed bool
}

func (t *trackedCloser) Close() error { t.closed = true; return nil }

// buildArchive writes a real zip containing the given entries and returns its bytes plus a
// parsed reader over them.
func buildArchive(t *testing.T, entries []zipTestEntry) ([]byte, *zip.Reader) {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: e.name, Method: e.method})
		if err != nil {
			t.Fatalf("create %s: %v", e.name, err)
		}
		if _, err := io.WriteString(w, e.body); err != nil {
			t.Fatalf("write %s: %v", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}

	raw := buf.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("parse archive: %v", err)
	}
	return raw, zr
}

type zipTestEntry struct {
	name   string
	body   string
	method uint16
}

func readAll(t *testing.T, rc io.ReadCloser) string {
	t.Helper()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

// A Slack daily file is JSON, and JSON deflates well, so Deflate is the case that matters.
// Store and empty entries appear too. Every one must produce exactly what archive/zip does.
func TestOpenZipEntryDirectMatchesArchiveZip(t *testing.T) {
	// Repetitive JSON so Deflate genuinely compresses and compressed != uncompressed size,
	// which is what makes an offset or length mistake visible.
	bigJSON := `[` + strings.Repeat(`{"type":"message","text":"hello there","user":"U1"},`, 500) + `{"type":"message"}]`

	entries := []zipTestEntry{
		{name: "channels.json", body: `[{"name":"general"}]`, method: zip.Deflate},
		{name: "general/2024-01-01.json", body: bigJSON, method: zip.Deflate},
		{name: "stored/2024-01-02.json", body: `[{"type":"message","text":"stored"}]`, method: zip.Store},
		// Store, not Deflate: deflating an empty string still produces a few bytes, so a
		// Deflate entry never has CompressedSize64 == 0 and would not reach the
		// no-request branch at all. Store does.
		{name: "general/empty-stored.json", body: "", method: zip.Store},
		{name: "general/empty-deflated.json", body: "", method: zip.Deflate},
		{name: "general/2024-01-03.json", body: bigJSON, method: zip.Store},
	}

	raw, zr := buildArchive(t, entries)

	for _, zf := range zr.File {
		// Ground truth: what archive/zip itself yields.
		std, err := zf.Open()
		if err != nil {
			t.Fatalf("%s: zf.Open: %v", zf.Name, err)
		}
		want := readAll(t, std)

		ranger := &memRanger{data: raw}
		fast, err := openZipEntryDirect(context.Background(), ranger, zf)
		if err != nil {
			t.Fatalf("%s: openZipEntryDirect: %v", zf.Name, err)
		}
		got := readAll(t, fast)

		if got != want {
			t.Errorf("%s: fast path returned %d bytes, archive/zip returned %d — contents differ",
				zf.Name, len(got), len(want))
			continue
		}

		// An empty entry must not issue a request at all.
		if zf.CompressedSize64 == 0 {
			if len(ranger.calls) != 0 {
				t.Errorf("%s: empty entry issued %d range requests, want 0", zf.Name, len(ranger.calls))
			}
			continue
		}

		// One request per entry is the whole point.
		if len(ranger.calls) != 1 {
			t.Errorf("%s: issued %d range requests, want exactly 1", zf.Name, len(ranger.calls))
			continue
		}

		// And it must ask for precisely this entry's compressed bytes — not one byte more,
		// which would read into the next entry's local header.
		off, err := zf.DataOffset()
		if err != nil {
			t.Fatalf("%s: DataOffset: %v", zf.Name, err)
		}
		wantCall := memRange{off: off, length: int64(zf.CompressedSize64)}
		if ranger.calls[0] != wantCall {
			t.Errorf("%s: requested range %+v, want %+v", zf.Name, ranger.calls[0], wantCall)
		}
	}
}

// Closing the returned reader must also close the ranged body underneath. The Deflate path
// wraps the body in a flate reader whose Close does not reach through, which is the entire
// reason compositeCloser exists — so it is worth proving rather than assuming.
func TestOpenZipEntryDirectClosesUnderlyingBody(t *testing.T) {
	for _, method := range []struct {
		name   string
		method uint16
	}{
		{"deflate", zip.Deflate},
		{"store", zip.Store},
	} {
		raw, zr := buildArchive(t, []zipTestEntry{
			{name: "general/2024-01-01.json", body: strings.Repeat(`{"a":"b"},`, 200), method: method.method},
		})

		ranger := &memRanger{data: raw}
		rc, err := openZipEntryDirect(context.Background(), ranger, zr.File[0])
		if err != nil {
			t.Fatalf("%s: %v", method.name, err)
		}
		if _, err := io.ReadAll(rc); err != nil {
			t.Fatalf("%s: read: %v", method.name, err)
		}
		if err := rc.Close(); err != nil {
			t.Errorf("%s: close: %v", method.name, err)
		}

		if len(ranger.handles) != 1 {
			t.Fatalf("%s: expected 1 body handle, got %d", method.name, len(ranger.handles))
		}
		if !ranger.handles[0].closed {
			t.Errorf("%s: closing the entry reader left the ranged body open — that leaks a "+
				"connection per entry, and an import opens one per daily file", method.name)
		}
	}
}

// An Archive with no ranged source is the disk-staged fallback. It must still read correctly,
// through archive/zip, without reaching for a fetcher that is not there.
func TestArchiveWithoutRangedSourceFallsBackToArchiveZip(t *testing.T) {
	body := `[{"type":"message","text":"from disk"}]`
	_, zr := buildArchive(t, []zipTestEntry{
		{name: "general/2024-01-01.json", body: body, method: zip.Deflate},
	})

	arc := diskArchive(zr)
	rc, err := arc.OpenEntry(context.Background(), zr.File[0])
	if err != nil {
		t.Fatalf("OpenEntry: %v", err)
	}
	if got := readAll(t, rc); got != body {
		t.Errorf("disk fallback returned %q, want %q", got, body)
	}
}

// A nil Archive is what a caller has when the archive was never opened; reading must fall
// back rather than panic, because IterMessages documents nil as valid.
func TestNilArchiveOpensEntryThroughArchiveZip(t *testing.T) {
	body := `[{"type":"message"}]`
	_, zr := buildArchive(t, []zipTestEntry{
		{name: "general/2024-01-01.json", body: body, method: zip.Deflate},
	})

	var arc *Archive
	rc, err := arc.OpenEntry(context.Background(), zr.File[0])
	if err != nil {
		t.Fatalf("OpenEntry on nil archive: %v", err)
	}
	if got := readAll(t, rc); got != body {
		t.Errorf("nil archive returned %q, want %q", got, body)
	}
}

// The whole point of the fast path is fewer requests. Read every entry of a multi-entry
// archive through one Archive and confirm the count equals the number of non-empty entries —
// no per-32 KB amplification, whatever the entry size.
func TestFastPathIssuesOneRequestPerEntry(t *testing.T) {
	// Deliberately larger than archive/zip's internal buffer so the old path would have
	// needed many reads for this one entry.
	huge := `[` + strings.Repeat(`{"type":"message","text":"a slack message that is reasonably long"},`, 4000) + `{}]`

	entries := []zipTestEntry{
		{name: "general/2024-01-01.json", body: huge, method: zip.Deflate},
		{name: "general/2024-01-02.json", body: huge, method: zip.Store},
		{name: "random/2024-01-03.json", body: `[{"type":"message"}]`, method: zip.Deflate},
	}
	raw, zr := buildArchive(t, entries)

	ranger := &memRanger{data: raw}
	arc := &Archive{Reader: zr, src: ranger}

	for _, zf := range zr.File {
		rc, err := arc.OpenEntry(context.Background(), zf)
		if err != nil {
			t.Fatalf("%s: %v", zf.Name, err)
		}
		if _, err := io.ReadAll(rc); err != nil {
			t.Fatalf("%s: read: %v", zf.Name, err)
		}
		rc.Close()
	}

	if len(ranger.calls) != len(entries) {
		t.Errorf("read %d entries in %d range requests, want one each",
			len(entries), len(ranger.calls))
	}
}

// fetchRange's bounds checks reject impossible ranges before any network call, so they can be
// verified here. A ReaderAt that asked for bytes past the object would produce a confusing
// MinIO error instead of a clear one.
func TestMinioFetchRangeRejectsImpossibleRanges(t *testing.T) {
	r := &minioReaderAt{bucket: "b", key: "k", size: 1000}

	cases := []struct {
		name        string
		off, length int64
	}{
		{"zero length", 0, 0},
		{"negative length", 10, -5},
		{"negative offset", -1, 10},
		{"runs past the end", 995, 10},
		{"starts past the end", 1000, 1},
	}

	for _, c := range cases {
		// No MinIO client is configured in unit tests; these must fail on the guard, which
		// is exactly what reaching the client would prove they do not.
		if _, err := r.fetchRange(context.Background(), c.off, c.length); err == nil {
			t.Errorf("%s: fetchRange(%d, %d) returned no error, want a bounds error",
				c.name, c.off, c.length)
		}
	}
}

// nopWriteCloser lets a test register a passthrough compressor under a made-up method id.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// A compression method the fast path does not implement must fall back to archive/zip rather
// than guess. The doc calls this out as protection against future Slack export formats, and
// it is the one branch that silently returns different bytes if it goes wrong — so it is
// worth exercising with an archive that genuinely uses an unknown method.
func TestUnknownCompressionMethodFallsBackToArchiveZip(t *testing.T) {
	const oddMethod uint16 = 99
	body := `[{"type":"message","text":"encoded by some future slack"}]`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	zw.RegisterCompressor(oddMethod, func(w io.Writer) (io.WriteCloser, error) {
		return nopWriteCloser{w}, nil
	})
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "general/2024-01-01.json", Method: oddMethod})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	raw := buf.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	zr.RegisterDecompressor(oddMethod, func(r io.Reader) io.ReadCloser {
		return io.NopCloser(r)
	})

	if zr.File[0].Method != oddMethod {
		t.Fatalf("archive did not preserve the unusual method, got %d", zr.File[0].Method)
	}

	ranger := &memRanger{data: raw}
	rc, err := openZipEntryDirect(context.Background(), ranger, zr.File[0])
	if err != nil {
		t.Fatalf("openZipEntryDirect: %v", err)
	}
	if got := readAll(t, rc); got != body {
		t.Errorf("unknown method returned %q, want %q", got, body)
	}

	// The fallback goes through archive/zip, so it must not have issued a ranged request of
	// its own.
	if len(ranger.calls) != 0 {
		t.Errorf("fallback issued %d range requests, want 0", len(ranger.calls))
	}
}
