package helpers

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// readinessCheckTimeout bounds each dependency probe /health/ready runs.
const readinessCheckTimeout = 8 * time.Second

// ServeReadiness answers /health/ready: every dependency check, one line each,
// "ok    <name>" (with a note in brackets when there is one) or
// "FAIL  <name>: <why>", and 200 when every check passes, 503 when any fails.
//
// WHY. make verify used to judge the backing services by grepping the API's
// startup log for "Successfully connected …" lines: a line from a run that has
// since lost the service still passed, and once the log had moved on (an
// exporter retrying every second pushed them out within minutes) a healthy
// install read FAIL. This asks the running server, now, through the same
// checks Admin › System checks shows. verify prints it and wait-ready waits on
// it (make api-ready), so the two cannot disagree about what ready means.
//
// ONLY TO THIS MACHINE. It names which services are down and why, which is
// nobody else's business: make reaches it through docker compose exec, from
// inside the container, and anyone else, Traefik included, gets a 404.
func ServeReadiness(w http.ResponseWriter, r *http.Request) {
	if !fromLoopback(r) {
		http.NotFound(w, r)
		return
	}
	results := RunDependencyChecks(r.Context(), readinessCheckTimeout)
	body, ready := readinessReport(results)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_, _ = w.Write([]byte(body))
}

// readinessReport writes the results one line each and says whether all passed.
func readinessReport(results []SystemCheckResult) (string, bool) {
	var b strings.Builder
	ready := len(results) > 0
	for _, res := range results {
		switch {
		case !res.Healthy:
			ready = false
			fmt.Fprintf(&b, "FAIL  %s: %s\n", res.Name, flattenDetail(res.Detail))
		case res.Detail != "":
			fmt.Fprintf(&b, "ok    %s (%s)\n", res.Name, flattenDetail(res.Detail))
		default:
			fmt.Fprintf(&b, "ok    %s\n", res.Name)
		}
	}
	if len(results) == 0 {
		b.WriteString("FAIL  checks: none registered, so nothing says this server is ready\n")
	}
	return b.String(), ready
}

func flattenDetail(s string) string { return strings.Join(strings.Fields(s), " ") }

// fromLoopback reports whether the request's connection came from this
// machine. RemoteAddr is the TCP peer; nothing in front of the router rewrites
// it from forwarded headers, which a caller could set.
func fromLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
