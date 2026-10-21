package business

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The policy's style source is the hash of the very style the documents
// carry, so the style applies and nothing else can.
func TestTheStyleSourceIsTheDocumentsOwnStyle(t *testing.T) {
	at := time.Date(2026, 10, 18, 9, 5, 0, 0, time.UTC)
	for name, d := range map[string]Document{
		"page":    PageDocument(&Page{Title: "Launch notes", HTML: "<p>Hi</p>"}, "https://api.example.test/public/p/launch-notes", "", "/public/p/launch-notes", at),
		"missing": MissingDocument(), "gone": GoneDocument(), "busy": BusyDocument(),
		"locked": LockedDocument("https://app.example.test/p/launch-notes?unlock=1"), "unavailable": UnavailableDocument(),
	} {
		m := regexp.MustCompile(`(?s)<style>(.*?)</style>`).FindSubmatch(d.Body)
		if m == nil {
			t.Fatalf("%s has no style", name)
		}
		sum := sha256.Sum256(m[1])
		if got := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"; got != StyleSource {
			t.Errorf("%s's style hashes to %s, the policy names %s", name, got, StyleSource)
		}
		if n := strings.Count(string(d.Body), "<style>"); n != 1 {
			t.Errorf("%s carries %d styles", name, n)
		}
		low := strings.ToLower(string(d.Body))
		for _, bad := range []string{"<script", "<form", "<iframe", "<link rel=\"stylesheet", "http://", "style=\""} {
			if strings.Contains(low, bad) {
				t.Errorf("%s carries %q", name, bad)
			}
		}
	}
}

// Indexing is off unless the publisher turns it on; the canonical link names
// the page's own address (D-25, condition 6).
func TestAPageIsFoundOnlyWhenItsPublisherSays(t *testing.T) {
	at := time.Date(2026, 10, 18, 9, 5, 0, 0, time.UTC)
	p := &Page{Title: "Launch notes", HTML: "<p>Hi</p>", UpdatedAt: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)}
	d := PageDocument(p, "https://api.example.test/public/p/launch-notes", "", "/public/p/launch-notes", at)
	body := string(d.Body)
	if d.Indexable || !strings.Contains(body, `<meta name="robots" content="noindex, nofollow">`) {
		t.Errorf("a page is noindex unless its publisher says:\n%s", body)
	}
	if !strings.Contains(body, `<link rel="canonical" href="https://api.example.test/public/p/launch-notes">`) {
		t.Errorf("the canonical link names the page's address:\n%s", body)
	}
	if !strings.Contains(body, `<p class="fresh">Updated 5 minutes ago · <a href="/public/p/launch-notes">Reload</a> for the latest</p>`) ||
		strings.Contains(body, "Copy to my workspace") {
		t.Errorf("the page says how fresh it is, and offers no copy unless allowed:\n%s", body)
	}
	if d.Status != http.StatusOK || !d.Revalidate || d.Private || !strings.HasPrefix(d.ETag, `"`) {
		t.Errorf("a page may be kept only if a cache asks first: %+v", d)
	}

	p.Indexable = true
	d2 := PageDocument(p, "https://api.example.test/public/p/launch-notes", "https://app.example.test/p/launch-notes?copy=1", "/public/p/launch-notes", at)
	if !d2.Indexable || !strings.Contains(string(d2.Body), `<meta name="robots" content="index, follow">`) {
		t.Errorf("a page its publisher lets search engines find says so:\n%s", d2.Body)
	}
	if !strings.Contains(string(d2.Body), `<a href="https://app.example.test/p/launch-notes?copy=1">Copy to my workspace</a>`) {
		t.Errorf("a page that may be copied offers it:\n%s", d2.Body)
	}
	if d2.ETag == d.ETag {
		t.Error("different bytes keep the same ETag")
	}
	if again := PageDocument(p, "https://api.example.test/public/p/launch-notes", "https://app.example.test/p/launch-notes?copy=1", "/public/p/launch-notes", at); again.ETag != d2.ETag {
		t.Error("the same bytes name a different ETag")
	}

	// A password page is kept by no one.
	p.Token = "1.2.sig"
	if d3 := PageDocument(p, "", "", "/public/p/launch-notes", at); !d3.Private {
		t.Errorf("a password page is private: %+v", d3)
	}
}

// The page draws the doc's look: its emoji, its cover, its font and width.
func TestThePageWearsTheDocsLook(t *testing.T) {
	at := time.Date(2026, 10, 18, 9, 5, 0, 0, time.UTC)
	body := string(PageDocument(&Page{Title: "Launch", HTML: "<p>Hi</p>", Icon: "🚀", Cover: "dusk", Font: "serif", SmallText: true, FullWidth: true}, "", "", "/public/p/launch-notes", at).Body)
	for _, want := range []string{`<body class="serif small wide">`, `<div class="cover cover-dusk"></div>`, `<div class="icon">🚀</div>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page misses %s:\n%s", want, body)
		}
	}
	body = string(PageDocument(&Page{Title: "Launch", Cover: "dusk", CoverURL: "/public/p/launch/files/" + fileA, Font: "comic"}, "", "", "/public/p/launch-notes", at).Body)
	if !strings.Contains(body, `<div class="cover"><img src="/public/p/launch/files/`+fileA+`" alt=""></div>`) || strings.Contains(body, "comic") ||
		strings.Contains(body, `class="cover cover-dusk"`) || strings.Contains(body, "<body class") {
		t.Errorf("an uploaded cover is the doc's picture; an unknown font is the default:\n%s", body)
	}
	if body := string(PageDocument(&Page{Title: "Launch", Cover: "unknown"}, "", "", "/public/p/launch-notes", at).Body); strings.Contains(body, `class="cover`) {
		t.Errorf("an unknown cover draws nothing:\n%s", body)
	}
}

// The calm pages say nothing of any doc, and are kept by no one (D-25,
// condition 7).
func TestTheCalmPagesSayNothingOfAnyDoc(t *testing.T) {
	if a, b := MissingDocument(), MissingDocument(); string(a.Body) != string(b.Body) || a.Status != http.StatusNotFound {
		t.Error("a missing page isn't always the same")
	}
	for name, d := range map[string]Document{"missing": MissingDocument(), "gone": GoneDocument(), "busy": BusyDocument(), "locked": LockedDocument("u")} {
		if d.Revalidate || d.ETag != "" || d.Indexable {
			t.Errorf("%s may be kept or found: %+v", name, d)
		}
		if !strings.Contains(string(d.Body), `content="noindex, nofollow"`) {
			t.Errorf("%s is indexable", name)
		}
	}
	if d := LockedDocument("https://app.example.test/p/x?unlock=1"); d.Status != http.StatusUnauthorized ||
		!strings.Contains(string(d.Body), `<a class="action" href="https://app.example.test/p/x?unlock=1">Enter the password</a>`) {
		t.Errorf("a password page sends the reader to the app to unlock it:\n%s", d.Body)
	}
	if d := GoneDocument(); d.Status != http.StatusGone {
		t.Errorf("an expired page answers %d", d.Status)
	}
}

// A page is a snapshot, and says how fresh it is, with a plain link that
// reloads it (its unlock kept on a password page).
func TestAPageSaysHowFreshItIs(t *testing.T) {
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	for updated, want := range map[time.Time]string{
		at.Add(-20 * time.Second):    "just now",
		at.Add(-1 * time.Minute):     "1 minute ago",
		at.Add(-5 * time.Minute):     "5 minutes ago",
		at.Add(-1 * time.Hour):       "1 hour ago",
		at.Add(-3 * time.Hour):       "3 hours ago",
		at.Add(-30 * time.Hour):      "yesterday",
		at.Add(-4 * 24 * time.Hour):  "4 days ago",
		at.Add(-40 * 24 * time.Hour): "on 8 September 2026",
	} {
		if got := ago(updated, at); got != want {
			t.Errorf("ago(%v) = %q, want %q", at.Sub(updated), got, want)
		}
	}
	if ago(time.Time{}, at) != "" {
		t.Error("a page with no time says one")
	}
	d := PageDocument(&Page{Title: "Launch", UpdatedAt: at.Add(-5 * time.Minute), Token: "3.99.sig"}, "", "", "/public/d/tok?k=3.99.sig", at)
	if !strings.Contains(string(d.Body), `Updated 5 minutes ago · <a href="/public/d/tok?k=3.99.sig">Reload</a> for the latest`) {
		t.Errorf("the freshness line:\n%s", d.Body)
	}
	if strings.Contains(string(d.Body), "<link rel=\"canonical\"") {
		t.Error("a link's token page names a canonical address")
	}
}
