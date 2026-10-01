package controllers

// Content-Security-Policy violation reports, received from the browser.
//
// WHY THIS EXISTS. The frontend ships its CSP as Content-Security-Policy-
// Report-Only, and the comment on that policy describes a validation window:
// watch what WOULD have been blocked, correct the policy, then enforce it. That
// window was collecting nothing. The policy named no reporting endpoint at all,
// so every violation went to the individual visitor's browser console and
// nowhere else. Report-Only with no collector is not a validation window, it is
// a header that does nothing while looking like a plan.
//
// WHY THE REPORTS COME HERE RATHER THAN TO US. OneCamp is self-hosted, and the
// whole argument for it is that a customer's data stays on their infrastructure.
// Shipping a policy that posts violation reports to onemana.dev would mean every
// install quietly telling us which pages its users visit and what those pages
// tried to load. The collector is the customer's own API. We never see them, and
// there is no build-time switch that would let us.
//
// TWO WIRE FORMATS, ONE HANDLER. Browsers disagree about how to send these:
//
//   report-uri    Content-Type: application/csp-report
//                 {"csp-report": {"document-uri": ..., "blocked-uri": ...}}
//                 Deprecated, and still the only thing Firefox and Safari send.
//
//   report-to     Content-Type: application/reports+json
//                 [{"type":"csp-violation","body":{"documentURL":...,...}}]
//                 Batched and retried. Chrome and Edge.
//
// The policy advertises both, because neither covers every browser on its own,
// so this accepts both and normalises them into one shape.
//
// IT IS AN OPEN ENDPOINT. It has to be: the browser posts these without
// credentials, and a violation on the sign-in page happens before anybody is
// authenticated. So it is rate limited, size capped, and it answers 204 to
// everything. It never reads the body into an unbounded buffer and never tells a
// caller whether their payload was understood.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	configModels "github.com/akashc777/OneCamp/models/postgres/Config"
)

// maxCSPReportBytes caps one request. A genuine report is well under a kilobyte;
// browsers batch a handful at a time. 64KB is generous for that and small enough
// that an open endpoint cannot be used to make the server allocate.
const maxCSPReportBytes = 64 << 10

// maxReportsPerRequest bounds a report-to batch. Without it, one request could
// carry thousands of entries and turn a single rate-limited call into thousands
// of log lines.
const maxReportsPerRequest = 20

// cspViolation is the normalised shape both wire formats are reduced to. Only
// the fields worth acting on: which page, what it tried to load, and which rule
// stopped it. The rest of the payload is noise for this purpose.
type cspViolation struct {
	DocumentURI string
	BlockedURI  string
	Directive   string
	Disposition string
}

// reportURIPayload is the deprecated format, still sent by Firefox and Safari.
type reportURIPayload struct {
	CSPReport struct {
		DocumentURI        string `json:"document-uri"`
		BlockedURI         string `json:"blocked-uri"`
		EffectiveDirective string `json:"effective-directive"`
		ViolatedDirective  string `json:"violated-directive"`
		Disposition        string `json:"disposition"`
	} `json:"csp-report"`
}

// reportToPayload is the Reporting API format: an array, because the browser
// batches.
type reportToPayload []struct {
	Type string `json:"type"`
	Body struct {
		DocumentURL        string `json:"documentURL"`
		BlockedURL         string `json:"blockedURL"`
		EffectiveDirective string `json:"effectiveDirective"`
		Disposition        string `json:"disposition"`
	} `json:"body"`
}

// ReceiveCSPReport handles POST /public/csp-report.
//
// Always 204, whatever arrives. A browser is not a client that can act on an
// error, and a parse failure here must not become a retry loop or a signal to
// somebody probing the endpoint.
func ReceiveCSPReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCSPReportBytes))
	if err != nil {
		// Oversized or truncated. Nothing to say to the browser.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	for _, v := range parseCSPReports(body, r.Header.Get("Content-Type")) {
		// Warn, not error: a violation is information about the policy, not a
		// fault in this service. Errors page people; this should not.
		helpers.LogWarnWithContext(ctx,
			"CSP %s: %s blocked %s on %s",
			v.Disposition, v.Directive, v.BlockedURI, v.DocumentURI)

		// Persisted so the validation window survives a redeploy. The log line
		// above is for whoever is watching right now; this is for deciding, in a
		// fortnight, whether the policy is safe to enforce.
		//
		// A failure here is not worth telling the browser about, and not worth
		// stopping the loop: one unrecorded violation is a smaller loss than
		// dropping the rest of the batch.
		if err := configModels.RecordCSPViolation(ctx,
			v.Directive,
			normaliseBlocked(v.BlockedURI),
			normaliseDocument(v.DocumentURI),
			v.Disposition,
		); err != nil {
			helpers.LogErrorWithContext(ctx, "CSP violation not recorded: %v", err)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// parseCSPReports turns either wire format into violations, and returns nothing
// for anything it cannot read.
//
// Content-Type decides which shape to TRY FIRST, not which to accept. Browsers
// have been inconsistent about this header, and a report that arrives with the
// wrong label is still a report; falling back costs one failed unmarshal.
func parseCSPReports(body []byte, contentType string) []cspViolation {
	if len(body) == 0 {
		return nil
	}

	preferBatch := strings.Contains(strings.ToLower(contentType), "reports+json")

	if preferBatch {
		if out := parseReportTo(body); len(out) > 0 {
			return out
		}
		return parseReportURI(body)
	}
	if out := parseReportURI(body); len(out) > 0 {
		return out
	}
	return parseReportTo(body)
}

func parseReportURI(body []byte) []cspViolation {
	var p reportURIPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil
	}
	// effective-directive is the specific rule; violated-directive is the older
	// field and often the whole directive with its value. Prefer the specific one.
	directive := helpers.FirstNonBlank(p.CSPReport.EffectiveDirective, p.CSPReport.ViolatedDirective)
	if p.CSPReport.DocumentURI == "" && directive == "" {
		return nil // parsed as JSON, but not a report
	}
	return []cspViolation{{
		DocumentURI: p.CSPReport.DocumentURI,
		BlockedURI:  p.CSPReport.BlockedURI,
		Directive:   directive,
		Disposition: helpers.FirstNonBlank(p.CSPReport.Disposition, "report"),
	}}
}

func parseReportTo(body []byte) []cspViolation {
	var p reportToPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil
	}
	out := make([]cspViolation, 0, len(p))
	for _, e := range p {
		// The Reporting API multiplexes: deprecation and intervention reports
		// arrive on the same endpoint group. Only CSP is ours to log.
		if e.Type != "csp-violation" {
			continue
		}
		if len(out) >= maxReportsPerRequest {
			break
		}
		out = append(out, cspViolation{
			DocumentURI: e.Body.DocumentURL,
			BlockedURI:  e.Body.BlockedURL,
			Directive:   e.Body.EffectiveDirective,
			Disposition: helpers.FirstNonBlank(e.Body.Disposition, "report"),
		})
	}
	return out
}

// normaliseBlocked reduces what was blocked to the thing an allowlist is written
// in: a scheme and a host.
//
// WHY ORIGIN AND NOT THE URL. A CSP permits https://example.com, never one path
// on it, so a path adds nothing an operator can act on and costs a row per
// distinct URL. The first real violation collected here was Cloudflare's own
// analytics beacon, whose path carries a build hash — keeping the full URL would
// have created a fresh "new" violation on every Cloudflare deploy, forever.
//
// Non-URL values are kept verbatim. A browser reports "inline", "eval" and
// "data" for those cases, and they are exactly the ones worth seeing.
func normaliseBlocked(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "(none)"
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme == "" || u.Host == "" {
		// "inline", "eval", "data", or something unparseable. Bounded so a
		// hostile payload cannot make the row itself the attack.
		return clip(v, 200)
	}
	return u.Scheme + "://" + u.Host
}

// normaliseDocument reduces where it happened to a path, dropping the query and
// fragment.
//
// A query string is where session ids, tokens and search terms live. None of it
// helps decide whether to allow an origin, and storing it would turn a security
// table into somewhere personal data accumulates without anyone deciding it
// should.
func normaliseDocument(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "(unknown)"
	}
	u, err := url.Parse(v)
	if err != nil {
		return clip(v, 200)
	}
	path := u.Path
	if path == "" {
		path = "/"
	}
	return clip(path, 200)
}

// clip bounds a stored string. Everything here arrives from an open endpoint.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// firstNonEmpty returns the first value that is not blank, so a caller can state
// a preference order without nesting conditionals at each site.
