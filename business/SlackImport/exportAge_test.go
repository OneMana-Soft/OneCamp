package business

import (
	"strings"
	"testing"
	"time"

	importAdapter "github.com/akashc777/OneCamp/adapter/SlackImport"
)

func TestNewestExportDay(t *testing.T) {
	cases := []struct {
		name  string
		files map[string][]string
		want  string
		ok    bool
	}{
		{
			name: "the latest day across every channel",
			files: map[string][]string{
				"C1": {"general/2026-01-02.json", "general/2026-03-14.json"},
				"C2": {"random/2026-02-01.json"},
			},
			want: "2026-03-14", ok: true,
		},
		{
			name:  "a DM directory is named by id, not by channel name",
			files: map[string][]string{"D1": {"D0123ABC/2026-05-09.json"}},
			want:  "2026-05-09", ok: true,
		},
		{
			name:  "manifests carry no date and must not be mistaken for one",
			files: map[string][]string{"C1": {"users.json", "channels.json"}},
			ok:    false,
		},
		{name: "an empty archive", files: map[string][]string{}, ok: false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := newestExportDay(c.files)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if !c.ok {
				return
			}
			if got.Format("2006-01-02") != c.want {
				t.Fatalf("newest = %s, want %s", got.Format("2006-01-02"), c.want)
			}
		})
	}
}

// The warning that stops someone importing an export whose files are already
// gone. Slack ships URLs rather than bytes, so a stale export imports every
// message, fetches no attachment, and finishes green.
func TestWarnIfFileLinksHaveExpired(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	day := func(d string) map[string][]string {
		return map[string][]string{"C1": {"general/" + d + ".json"}}
	}

	cases := []struct {
		name      string
		files     int
		exported  string
		wantWarn  bool
		wantWords string
	}{
		{"fresh export", 20, "2026-08-30", false, ""},
		{"approaching the window", 20, "2026-06-15", true, "run this soon"},
		{"past the window", 20, "2026-01-05", true, "probably fail"},
		// No attachments, no problem worth interrupting anybody for.
		{"old export with no files at all", 0, "2026-01-05", false, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := &importAdapter.PlanResponse{FileCount: c.files}
			warnIfFileLinksHaveExpired(resp, &ParsedExport{MessageFiles: day(c.exported)}, now)

			if !c.wantWarn {
				if len(resp.Warnings) != 0 {
					t.Fatalf("expected no warning, got %v", resp.Warnings)
				}
				return
			}
			if len(resp.Warnings) != 1 {
				t.Fatalf("expected one warning, got %v", resp.Warnings)
			}
			if !strings.Contains(resp.Warnings[0], c.wantWords) {
				t.Fatalf("warning does not say %q: %s", c.wantWords, resp.Warnings[0])
			}
		})
	}
}

// The plan and the downloader must not disagree about the deadline.
func TestPlanUsesTheDownloadersWindow(t *testing.T) {
	if SlackFileLinkWindow != 90*24*time.Hour {
		t.Fatalf("the file link window moved to %s; the warning text quotes it in days", SlackFileLinkWindow)
	}
}
