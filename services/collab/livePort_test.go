package collab

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The Live Doc Writer's port is internal: the API writes into any doc through
// it, with only the internal secret in front. Both compose files (ours, and the
// one customers install from) must give the collaboration service its live port
// and route nothing to it: Traefik's labels name the public port, 1234, alone.
func TestTheLivePortIsNeverRoutedPublicly(t *testing.T) {
	for _, file := range []string{"../../final-compose.yml", "../../distribute-compose.yml"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		block := serviceBlock(string(raw), "collaboration-service")
		if block == "" {
			t.Fatalf("%s has no collaboration-service", file)
		}
		if !strings.Contains(block, "- LIVE_PORT=1235") {
			t.Errorf("%s: the collaboration service has no LIVE_PORT=1235; the API writes to http://collaboration-service:1235", file)
		}
		if strings.Contains(block, "ports:") {
			t.Errorf("%s: the collaboration service publishes ports; nothing of it may be reachable from outside but Traefik's route to 1234", file)
		}
		for _, m := range regexp.MustCompile(`(?m)^\s*- traefik\.[^\n]*`).FindAllString(block, -1) {
			if strings.Contains(m, "1235") {
				t.Errorf("%s: a Traefik label names the live port: %s", file, strings.TrimSpace(m))
			}
			if strings.Contains(m, "loadbalancer.server.port") && !strings.Contains(m, "=1234") {
				t.Errorf("%s: Traefik routes the collaboration service to a port other than 1234: %s", file, strings.TrimSpace(m))
			}
		}
	}
}

// serviceBlock is one service's lines in a compose file.
func serviceBlock(compose, name string) string {
	lines := strings.Split(compose, "\n")
	var out []string
	in := false
	for _, l := range lines {
		if strings.HasPrefix(l, "  "+name+":") {
			in = true
			continue
		}
		if in && len(l) > 2 && l[0] == ' ' && l[1] == ' ' && l[2] != ' ' && l[2] != '#' {
			break
		}
		if in {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
