package helpers

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The application must wait for the stores it connects to at boot.
//
// WHAT WENT WRONG. go-service declared `depends_on: [traefik]` and nothing else,
// so it started the moment the proxy existed and raced Postgres, Dgraph, Redis,
// MinIO and the MQTT broker on every start. It connects to all of them at boot and
// EXITS if the broker is missing, so `restart: always` turned that race into a
// crash-loop — the demo host restarted nine times after a single restore before it
// settled.
//
// The loop was self-healing, which is why it survived: the stack came up
// eventually and nobody looked. What it cost was every check that asks whether the
// install is working. `make verify` reported FAIL on a restore that had worked,
// every night for seventeen nights, and a warning that is wrong every night is one
// nobody reads on the night it is right. I dismissed it as a false alarm myself
// before running it.
//
// Checked as text rather than by parsing YAML, because the repo's compose files
// are read by several guards that already take that approach and adding a YAML
// dependency for one assertion is not worth it.
func TestApplicationWaitsForItsDatastores(t *testing.T) {
	for _, file := range []string{"../distribute-compose.yml", "../final-compose.yml"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			// final-compose is the demo host's and may be absent from a carve.
			if strings.Contains(file, "final-compose") && os.IsNotExist(err) {
				continue
			}
			t.Fatalf("reading %s: %v", file, err)
		}
		body := string(raw)

		block := serviceBlock(t, body, "go-service")
		if block == "" {
			t.Fatalf("%s: no go-service; this guard is watching nothing", file)
		}
		if !strings.Contains(block, "depends_on:") {
			t.Errorf("%s: go-service has no depends_on, so it starts alongside its datastores "+
				"and crash-loops until they are ready", file)
			continue
		}
		// Traefik alone is the bug: the proxy is not a datastore.
		if regexp.MustCompile(`depends_on:\s*\n\s*- traefik\s*\n\s*\n`).MatchString(block) {
			t.Errorf("%s: go-service depends only on traefik. The proxy being up says nothing "+
				"about Postgres, Redis or the broker, which it needs at boot.", file)
		}
		for _, svc := range []string{"postgres", "redis"} {
			if !strings.Contains(block, svc+":\n        condition: service_healthy") {
				t.Errorf("%s: go-service does not wait for %s to be HEALTHY. Waiting for the "+
					"container to exist is not waiting for the store to answer.", file, svc)
			}
		}
		// The three the application also dials at boot. It EXITS when the broker is
		// missing, so emqx in particular is the difference between a slow start and
		// a crash-loop. Only service_started is available: none of them defines a
		// healthcheck, and ordering the start is still worth having.
		for _, svc := range []string{"alpha", "minio", "emqx"} {
			if !strings.Contains(block, svc+":\n        condition: service_") {
				t.Errorf("%s: go-service does not wait for %s at all. It connects to it at boot, "+
					"so a whole-stack restart races it and the application exits and retries.",
					file, svc)
			}
		}
	}
}

// A condition: service_healthy on a service with no healthcheck never becomes
// satisfiable, so the stack would refuse to start rather than merely being noisy.
func TestServicesWaitedOnForHealthDefineAHealthcheck(t *testing.T) {
	for _, file := range []string{"../distribute-compose.yml", "../final-compose.yml"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		body := string(raw)
		waited := regexp.MustCompile(`(?m)^\s{6}([a-z0-9_-]+):\n\s{8}condition: service_healthy`)
		for _, m := range waited.FindAllStringSubmatch(body, -1) {
			svc := m[1]
			if !strings.Contains(serviceBlock(t, body, svc), "healthcheck:") {
				t.Errorf("%s: something waits for %s to be healthy, but %s defines no "+
					"healthcheck — that condition can never be met and the stack will not start",
					file, svc, svc)
			}
		}
	}
}

// serviceBlock returns one service's YAML block from a compose file.
func serviceBlock(t *testing.T, body, svc string) string {
	t.Helper()
	start := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(svc) + `:\s*$`).FindStringIndex(body)
	if start == nil {
		return ""
	}
	rest := body[start[1]:]
	next := regexp.MustCompile(`(?m)^  [a-zA-Z0-9_-]+:\s*$`).FindStringIndex(rest)
	if next == nil {
		return rest
	}
	return rest[:next[0]]
}
