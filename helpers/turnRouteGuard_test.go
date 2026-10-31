package helpers

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TURN over TLS is routed where LiveKit tells callers to find it.
//
// WHAT WENT WRONG. LiveKit hands every caller turns:<TURN_DOMAIN>:443, the one
// way to relay through a firewall that lets only 443 out. On the host, 443 is
// Traefik's. Our own stack (final-compose.yml) gave Traefik a TCP router for
// that name, ending TLS and passing the stream to LiveKit's TURN port
// (livekit.yaml's tls_port, with external_tls: true). The compose file
// customers install had no such router, so on every install TURN over TLS
// answered with Traefik's default page. Calls failed for exactly the people
// TURN/TLS is for (found by DOCS-LEAD-2's C6-4a trace, 22 Oct).
func TestTURNIsRoutedWhereLiveKitSaysItIs(t *testing.T) {
	sample, err := os.ReadFile("../livekit.yaml.sample")
	if err != nil {
		t.Fatal(err)
	}
	turn := regexp.MustCompile(`(?m)^turn:\n((?:[ \t].*\n|\n)*)`).FindSubmatch(sample)
	if turn == nil {
		t.Fatal("livekit.yaml.sample has no turn: section; update this test to where TURN is set")
	}
	tlsPort := regexp.MustCompile(`(?m)^\s+tls_port:\s*(\d+)`).FindSubmatch(turn[1])
	if tlsPort == nil || !regexp.MustCompile(`(?m)^\s+external_tls:\s*true`).Match(turn[1]) {
		t.Fatalf("livekit.yaml.sample's TURN isn't a tls_port behind external TLS any more; check the routers below agree with it")
	}
	for _, f := range []string{"../distribute-compose.yml", "../final-compose.yml"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		c := string(b)
		if !strings.Contains(c, `LIVEKIT_TURN_URL: "turns:${TURN_DOMAIN}:443?transport=tcp"`) {
			t.Errorf("%s no longer sends callers to turns:${TURN_DOMAIN}:443; check this test against what it says now", f)
			continue
		}
		router := regexp.MustCompile(`traefik\.tcp\.routers\.([a-z0-9-]+)\.rule=HostSNI\(` + "`" + `\$\{TURN_DOMAIN\}` + "`" + `\)`).FindStringSubmatch(c)
		if router == nil {
			t.Errorf("%s has no TCP router for HostSNI(${TURN_DOMAIN}): TURN over TLS on 443 reaches nothing", f)
			continue
		}
		value := func(key string) string {
			m := regexp.MustCompile(regexp.QuoteMeta(key) + `=(\S+)`).FindStringSubmatch(c)
			if m == nil {
				return ""
			}
			return m[1]
		}
		name := router[1]
		if ep := value("traefik.tcp.routers." + name + ".entrypoints"); ep != "https" {
			t.Errorf("%s routes TURN on %q, but callers come on 443 (the https entrypoint)", f, ep)
		}
		if tls := value("traefik.tcp.routers." + name + ".tls"); tls != "true" {
			t.Errorf("%s's TURN router doesn't end TLS (tls=%q), and LiveKit's TURN port takes it ended (external_tls)", f, tls)
		}
		svc := value("traefik.tcp.routers." + name + ".service")
		if port := value("traefik.tcp.services." + svc + ".loadbalancer.server.port"); svc == "" || port != string(tlsPort[1]) {
			t.Errorf("%s passes TURN to service %q on port %q, but LiveKit's TURN listens on %s", f, svc, port, tlsPort[1])
		}
	}
}
