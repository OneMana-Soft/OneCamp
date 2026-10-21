package helpers

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// No proxy access log may record request paths.
//
// WHY. A path can be a private link: a calendar's secret feed
// (/public/calendar/{token}.ics), a guest's booking (/public/booking/{token}),
// an invitation's answer page (/public/rsvp/{token}), a guest's meeting link.
// Each is a bearer secret, so a log that keeps paths hands every one of them to
// whoever reads it, for as long as the log is kept. Traefik's default access
// log is the common format, whose request line always carries the path, so
// the log is JSON with RequestPath dropped: status, duration, method, host,
// router and service stay, so an operator still sees what failed where. Its
// headers stay dropped too, as Traefik's default. The API's own logs name
// routes by their patterns and never a path.
var (
	accessLogOn        = regexp.MustCompile(`^\s*-\s*"?--accesslog(?:=true)?"?\s*(?:#.*)?$`)
	accessLogFormat    = regexp.MustCompile(`--accesslog\.format=(\w+)`)
	accessLogDropsPath = regexp.MustCompile(`--accesslog\.fields\.names\.RequestPath=drop\b`)
	accessLogHeaders   = regexp.MustCompile(`--accesslog\.fields\.headers\.defaultmode=(\w+)`)
)

func TestNoProxyAccessLogKeepsRequestPaths(t *testing.T) {
	for _, path := range deployedComposeFiles {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		on, format, drops, headers := 0, "", false, ""
		for i, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if accessLogOn.MatchString(line) {
				on = i + 1
			}
			if m := accessLogFormat.FindStringSubmatch(line); m != nil {
				format = m[1]
			}
			if accessLogDropsPath.MatchString(line) {
				drops = true
			}
			if m := accessLogHeaders.FindStringSubmatch(line); m != nil {
				headers = m[1]
			}
		}
		if on == 0 {
			continue
		}
		if format != "json" {
			t.Errorf("%s:%d turns the proxy's access log on in the %q format, whose request line carries every path, private links included: use --accesslog.format=json with --accesslog.fields.names.RequestPath=drop",
				shortName(path), on, orDefault(format, "common"))
		}
		if !drops {
			t.Errorf("%s:%d turns the proxy's access log on without --accesslog.fields.names.RequestPath=drop: a path can be a private link (a calendar feed, a booking, an invitation), and the log would keep it",
				shortName(path), on)
		}
		if headers != "" && headers != "drop" {
			t.Errorf("%s:%d keeps the access log's headers (%s): they carry cookies and authorisation; leave them dropped", shortName(path), on, headers)
		}
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
