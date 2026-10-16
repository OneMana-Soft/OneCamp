package helpers

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The API's answers are compressed by the proxy in front of it.
//
// WHAT WENT WRONG. Neither the server nor the proxy in the shipped compose file
// compressed anything, so on a customer's own server a table opened with about
// 600 KB of JSON on the wire, and every refetch after an edit cost the same
// (TAB-BUG-13). The demo never showed it: Cloudflare compresses in front of it.
//
// Event streams are left alone. Measured on traefik:v3.0: a browser asks for
// Brotli, and with it the first words of the assistant's reply arrived 0.8 s
// after they were written. v3.7.13 sends a compressed stream on time, but a
// stream gains nothing from compression, so it stays out on every version.
func TestTheAPIsAnswersAreCompressed(t *testing.T) {
	uses := regexp.MustCompile(`traefik\.http\.routers\.go-service-https\.middlewares=([A-Za-z0-9_,@-]+)`)
	for _, f := range []string{"../distribute-compose.yml", "../final-compose.yml"} {
		c, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		c = uncommented(c)
		m := uses.FindSubmatch(c)
		if m == nil {
			t.Errorf("%s: the API's HTTPS router names no middleware, so its answers go out uncompressed", f)
			continue
		}
		var excluded []string
		compresses := false
		for _, name := range strings.Split(string(m[1]), ",") {
			def := regexp.MustCompile(`traefik\.http\.middlewares\.` + regexp.QuoteMeta(strings.TrimSuffix(name, "@docker")) +
				`\.compress(?:=true|\.excludedcontenttypes=([^\s]*))`)
			for _, d := range def.FindAllSubmatch(c, -1) {
				compresses = true
				excluded = append(excluded, strings.Split(string(d[1]), ",")...)
			}
		}
		switch {
		case !compresses:
			t.Errorf("%s: the API's HTTPS router (%s) has no compress middleware, so its answers go out uncompressed", f, m[1])
		case !slices.ContainsFunc(excluded, func(v string) bool { return strings.EqualFold(strings.TrimSpace(v), "text/event-stream") }):
			t.Errorf("%s: the API's compress middleware compresses event streams too, which holds the assistant's words back", f)
		}
	}
}

// uncommented is a YAML file without its comment lines: a label quoted in a
// comment configures nothing, and the guard read the first match, wherever.
func uncommented(c []byte) []byte {
	lines := strings.Split(string(c), "\n")
	kept := lines[:0]
	for _, l := range lines {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") {
			kept = append(kept, l)
		}
	}
	return []byte(strings.Join(kept, "\n"))
}

func TestACommentConfiguresNothing(t *testing.T) {
	c := uncommented([]byte("    labels:\n      # - traefik.http.routers.go-service-https.middlewares=gzip\n      - traefik.enable=true\n"))
	if strings.Contains(string(c), "middlewares") || !strings.Contains(string(c), "traefik.enable") {
		t.Errorf("read as %q", c)
	}
}
