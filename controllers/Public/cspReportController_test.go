package controllers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The collector exists because the CSP was Report-Only with nowhere to report
// to. These cover the part that decides whether a report is understood at all,
// because the failure mode being fixed is silence: a report that arrives and is
// dropped looks exactly like no violation happening.

func TestParsesTheDeprecatedReportURIFormat(t *testing.T) {
	// What Firefox and Safari send, and the only format they send.
	body := []byte(`{"csp-report":{
		"document-uri":"https://onecamp.acme.com/app",
		"blocked-uri":"https://evil.example/x.js",
		"effective-directive":"script-src",
		"disposition":"report"}}`)

	got := parseCSPReports(body, "application/csp-report")
	if len(got) != 1 {
		t.Fatalf("expected 1 violation, got %d", len(got))
	}
	if got[0].BlockedURI != "https://evil.example/x.js" || got[0].Directive != "script-src" {
		t.Errorf("wrong fields: %+v", got[0])
	}
}

func TestParsesTheReportingAPIBatch(t *testing.T) {
	// What Chrome and Edge send, batched.
	body := []byte(`[
		{"type":"csp-violation","body":{"documentURL":"https://onecamp.acme.com/a","blockedURL":"https://evil.example/1.js","effectiveDirective":"script-src"}},
		{"type":"csp-violation","body":{"documentURL":"https://onecamp.acme.com/b","blockedURL":"https://evil.example/2.js","effectiveDirective":"img-src"}}
	]`)

	got := parseCSPReports(body, "application/reports+json")
	if len(got) != 2 {
		t.Fatalf("expected 2 violations, got %d", len(got))
	}
	if got[1].Directive != "img-src" {
		t.Errorf("second entry wrong: %+v", got[1])
	}
}

func TestIgnoresNonCSPReportsOnTheSameEndpoint(t *testing.T) {
	// The Reporting API multiplexes: deprecation and intervention reports arrive
	// on the same endpoint group. Logging those as CSP violations would fill the
	// validation window with entries that have nothing to do with the policy.
	body := []byte(`[
		{"type":"deprecation","body":{"id":"x"}},
		{"type":"csp-violation","body":{"documentURL":"https://a/","blockedURL":"https://b/","effectiveDirective":"font-src"}}
	]`)

	got := parseCSPReports(body, "application/reports+json")
	if len(got) != 1 || got[0].Directive != "font-src" {
		t.Fatalf("expected only the csp-violation, got %+v", got)
	}
}

func TestReadsAReportSentWithTheWrongContentType(t *testing.T) {
	// Browsers have been inconsistent about this header, and a mislabelled report
	// is still a report. Content-Type picks which shape to try first, not which
	// to accept.
	uriShape := []byte(`{"csp-report":{"document-uri":"https://a/","effective-directive":"script-src"}}`)
	if got := parseCSPReports(uriShape, "application/reports+json"); len(got) != 1 {
		t.Errorf("report-uri payload labelled reports+json was dropped")
	}

	batchShape := []byte(`[{"type":"csp-violation","body":{"documentURL":"https://a/","effectiveDirective":"img-src"}}]`)
	if got := parseCSPReports(batchShape, "application/csp-report"); len(got) != 1 {
		t.Errorf("reports+json payload labelled csp-report was dropped")
	}
}

func TestRejectsThingsThatAreNotReports(t *testing.T) {
	// An open endpoint receives junk. None of it should become a log line.
	for _, body := range []string{"", "not json", "{}", "[]", `{"hello":"world"}`, `[{"type":"deprecation","body":{}}]`} {
		if got := parseCSPReports([]byte(body), "application/csp-report"); len(got) != 0 {
			t.Errorf("input %q produced %d violations", body, len(got))
		}
	}
}

func TestABatchCannotProduceUnboundedLogLines(t *testing.T) {
	// One rate-limited request must not turn into thousands of log lines.
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < maxReportsPerRequest*5; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"type":"csp-violation","body":{"documentURL":"https://a/","effectiveDirective":"script-src"}}`)
	}
	b.WriteString("]")

	if got := parseCSPReports([]byte(b.String()), "application/reports+json"); len(got) != maxReportsPerRequest {
		t.Errorf("expected the batch capped at %d, got %d", maxReportsPerRequest, len(got))
	}
}

func TestAlwaysAnswers204(t *testing.T) {
	// A browser cannot act on an error, and a non-2xx invites a retry loop or
	// tells somebody probing the endpoint that they found something.
	for _, body := range []string{
		`{"csp-report":{"document-uri":"https://a/","effective-directive":"script-src"}}`,
		"garbage",
		"",
		strings.Repeat("x", maxCSPReportBytes+1024), // over the cap
	} {
		req := httptest.NewRequest(http.MethodPost, "/public/csp-report", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/csp-report")
		rec := httptest.NewRecorder()

		ReceiveCSPReport(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Errorf("body %.20q returned %d, want 204", body, rec.Code)
		}
	}
}

// Normalisation is what keeps the violations table a readable set rather than an
// unbounded log. These pin the two decisions that do that work.

func TestBlockedUrisCollapseToTheirOrigin(t *testing.T) {
	// THE CASE THIS EXISTS FOR. The first real violation collected was
	// Cloudflare's analytics beacon, whose path carries a build hash. Keeping
	// the path would mint a brand new "violation" on every Cloudflare deploy,
	// forever, and bury the real ones.
	const a = "https://static.cloudflareinsights.com/beacon.min.js/v3d52b47920f24c319"
	const b = "https://static.cloudflareinsights.com/beacon.min.js/vAAAAAAAAAAAAAAAAAA"
	if normaliseBlocked(a) != normaliseBlocked(b) {
		t.Errorf("two builds of the same script produced different rows: %q vs %q",
			normaliseBlocked(a), normaliseBlocked(b))
	}
	if got := normaliseBlocked(a); got != "https://static.cloudflareinsights.com" {
		t.Errorf("got %q, want the bare origin", got)
	}
}

func TestOriginGranularityMatchesWhatAPolicyCanExpress(t *testing.T) {
	// A CSP permits an origin, never a path, so the row should be the thing an
	// operator would actually add to the allowlist.
	if got := normaliseBlocked("https://cdn.example.com/a/b/c.js?v=2#x"); got != "https://cdn.example.com" {
		t.Errorf("got %q, want https://cdn.example.com", got)
	}
	// Port is part of an origin and must survive.
	if got := normaliseBlocked("http://localhost:9000/file.png"); got != "http://localhost:9000" {
		t.Errorf("got %q, port was lost", got)
	}
}

func TestBrowserKeywordsAreKeptVerbatim(t *testing.T) {
	// "inline", "eval" and "data" are not URLs and are exactly the violations
	// worth seeing. Mangling them into "(none)" would hide the most important
	// rows in the table.
	for _, kw := range []string{"inline", "eval", "data", "blob"} {
		if got := normaliseBlocked(kw); got != kw {
			t.Errorf("keyword %q became %q", kw, got)
		}
	}
	if got := normaliseBlocked(""); got != "(none)" {
		t.Errorf("empty blocked-uri became %q", got)
	}
}

func TestDocumentPathsDropQueryAndFragment(t *testing.T) {
	// A query string is where session ids, tokens and search terms live. None of
	// it helps decide whether to allow an origin, and keeping it would turn a
	// security table into somewhere personal data accumulates unnoticed.
	got := normaliseDocument("https://onecamp.acme.com/app/channel/abc?token=SECRET&q=payroll#top")
	if got != "/app/channel/abc" {
		t.Errorf("got %q, want the bare path", got)
	}
	if strings.Contains(got, "SECRET") || strings.Contains(got, "payroll") {
		t.Error("query string survived into the stored row")
	}
	if got := normaliseDocument("https://onecamp.acme.com"); got != "/" {
		t.Errorf("a bare origin gave %q, want /", got)
	}
}

func TestStoredValuesAreBounded(t *testing.T) {
	// Everything here arrives from an open endpoint, so a hostile payload must
	// not become the row.
	long := "inline" + strings.Repeat("A", 5000)
	if len(normaliseBlocked(long)) > 250 {
		t.Errorf("blocked value stored at %d characters", len(normaliseBlocked(long)))
	}
	if len(normaliseDocument("/"+strings.Repeat("b", 5000))) > 250 {
		t.Error("document path was not bounded")
	}
}
