package business

import (
	"archive/zip"
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers/zipsafe"
)

// makeMinimalSlackExport produces an in-memory zip containing the
// bare minimum a real Slack export carries: users.json + channels.json.
func makeMinimalSlackExport(t *testing.T, users, channels string) *zip.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"users.json":    users,
		"channels.json": channels,
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	return zr
}

func TestParseManifests_AcceptsValid(t *testing.T) {
	users := `[{"id":"U1","name":"alice","profile":{"email":"a@b"}}]`
	channels := `[{"id":"C1","name":"general"}]`
	zr := makeMinimalSlackExport(t, users, channels)
	out, err := ParseManifests(zr)
	if err != nil {
		t.Fatalf("want ok, got %v", err)
	}
	if len(out.Users) != 1 || len(out.Channels) != 1 {
		t.Errorf("unexpected counts: users=%d channels=%d", len(out.Users), len(out.Channels))
	}
}

func TestParseManifests_RejectsZipBomb(t *testing.T) {
	// Create a small zip and then forge metadata so ValidateZip's
	// per-entry cap fires. We simulate the bomb by directly mutating
	// the parsed file's UncompressedSize64 — same effect as a header
	// that lies about the size.
	users := `[{"id":"U1","profile":{"email":""}}]`
	channels := `[{"id":"C1","name":"general"}]`
	zr := makeMinimalSlackExport(t, users, channels)
	// Forge an absurd size on one entry to trip the ratio/per-entry caps.
	zr.File[0].UncompressedSize64 = 11 * 1024 * 1024 * 1024 // 11 GiB
	zr.File[0].CompressedSize64 = 1                         // 11G/1 = 11G ratio
	_, err := ParseManifests(zr)
	if err == nil || !strings.Contains(err.Error(), "zip safety") {
		t.Fatalf("want zip safety error, got %v", err)
	}
	if !errors.Is(err, zipsafe.ErrEntryTooLarge) && !errors.Is(err, zipsafe.ErrCompressionRatio) {
		t.Logf("error type: %v", err)
	}
}

func TestParseManifests_RejectsUnsafePath(t *testing.T) {
	users := `[{"id":"U1","profile":{"email":""}}]`
	channels := `[{"id":"C1","name":"general"}]`
	zr := makeMinimalSlackExport(t, users, channels)
	zr.File[0].Name = "../escape.json"
	_, err := ParseManifests(zr)
	if err == nil {
		t.Fatalf("want error for unsafe path, got nil")
	}
}

func TestParseManifests_RejectsTooManyEntries(t *testing.T) {
	t.Setenv("SLACK_ZIP_MAX_ENTRIES", "1")
	users := `[{"id":"U1","profile":{"email":""}}]`
	channels := `[{"id":"C1","name":"general"}]`
	zr := makeMinimalSlackExport(t, users, channels)
	// 2 entries > limit of 1.
	_, err := ParseManifests(zr)
	if err == nil || !strings.Contains(err.Error(), "many entries") {
		t.Fatalf("want too-many-entries, got %v", err)
	}
}
