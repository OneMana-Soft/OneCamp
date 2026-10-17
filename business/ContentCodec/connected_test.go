package contentcodec

import (
	"strings"
	"testing"
)

// A date that is on no calendar ("2026-02-30") is dropped wherever a doc is
// read, so nothing that draws the day (the web's chip would throw) meets it.
func TestADateThatIsNoDayIsDropped(t *testing.T) {
	for raw, want := range map[string]string{
		"2026-02-30": "", "2026-13-01": "", "2026-1-2": "", "12 Oct": "",
		"2026-10-12": "2026-10-12", "2028-02-29": "2028-02-29",
	} {
		if got := isoDate(raw); got != want {
			t.Errorf("isoDate(%q) = %q, want %q", raw, got, want)
		}
	}
	doc, err := FromHTML(`<p>Due <time data-type="date-mention" datetime="2026-02-30">30 Feb</time></p>`, HTMLOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out := ToHTML(doc); strings.Contains(out, "2026-02-30") || !strings.Contains(out, `<time data-type="date-mention"></time>`) {
		t.Fatalf("the day that is none was kept: %s", out)
	}
	if got := dateWords("2026-10-01"); got != "1 Oct 2026" {
		t.Fatalf("dateWords = %q", got)
	}
}
