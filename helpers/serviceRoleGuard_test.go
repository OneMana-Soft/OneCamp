package helpers

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every background loop is started from startBackgroundLoops, and nowhere else.
//
// THE FAILURE THIS PREVENTS. A loop started from main or Serve directly runs on
// every replica, including a SERVICE_ROLE=api one, which is the one place it
// must not: two replicas would then both claim from the queue that only the
// worker was meant to drive, and the operator who set the role to keep agent
// CPU off the API tier would have achieved nothing, silently. The next loop
// will be added by somebody who has not read helpers.ServiceRole, so the rule
// is checked rather than remembered.
//
// The seeds and registries a request needs (the command catalog, the bot
// identity, the MCP registry) are deliberately NOT loops and stay outside; the
// config reconcilers keep a request-serving replica's caches honest and belong
// on every role. They are listed so the guard can tell them apart.
var startedOnEveryRole = map[string]bool{
	"aiBusiness.StartConfigReconcile":                  true, // cache coherence, every role
	"mcpBusiness.Start":                                true, // registry build, every role
	"mcpServerBusiness.StartPendingActionHousekeeping": true, // gated in place in main
}

func TestEveryBackgroundLoopIsStartedFromTheGatedFunction(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "cmd", "server", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)

	fnStart := strings.Index(s, "\nfunc startBackgroundLoops(")
	if fnStart < 0 {
		t.Fatal("startBackgroundLoops is missing; the role split depends on it")
	}
	fnEnd := fnStart + strings.Index(s[fnStart+1:], "\n}\n") + 1
	inside := s[fnStart:fnEnd]
	outside := s[:fnStart] + s[fnEnd:]

	// A call that starts something: pkg.Start...( or pkg.StartX(.
	call := regexp.MustCompile(`\b([A-Za-z]+\.Start[A-Za-z]*)\(`)

	for _, m := range call.FindAllStringSubmatch(outside, -1) {
		name := m[1]
		if startedOnEveryRole[name] {
			continue
		}
		t.Errorf("%s is started outside startBackgroundLoops, so it would run on an api-only replica. "+
			"Move it into startBackgroundLoops, or if it genuinely belongs on every role, add it to "+
			"startedOnEveryRole with the reason.", name)
	}

	// And the function is not empty, or the guard above is trivially true.
	if n := len(call.FindAllString(inside, -1)); n < 8 {
		t.Fatalf("startBackgroundLoops starts only %d things; the loops were expected here", n)
	}

	// The housekeeping loop stays in main where it exists (the AI edition), but
	// it must be behind the role. The AI-free edition has no such loop.
	if hk := strings.Index(outside, "mcpServerBusiness.StartPendingActionHousekeeping()"); hk >= 0 {
		before := outside[max(0, hk-120):hk]
		if !strings.Contains(before, "role.RunsWorkers()") {
			t.Error("StartPendingActionHousekeeping is a loop and must be gated on role.RunsWorkers()")
		}
	}
}

// A worker replica's HTTP surface is the health route and nothing else.
func TestWorkerServesOnlyHealth(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "cmd", "server", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, "func healthOnlyHandler()")
	if i < 0 {
		t.Fatal("healthOnlyHandler is missing")
	}
	body := s[i : i+strings.Index(s[i:], "\n}\n")]
	if strings.Count(body, "HandleFunc(") != 1 || !strings.Contains(body, `"GET /health"`) {
		t.Fatalf("a worker must mount exactly the health route:\n%s", body)
	}
	if !strings.Contains(s, "if !role.ServesHTTP() {\n\t\thandler = healthOnlyHandler()") {
		t.Fatal("Serve must swap in healthOnlyHandler when the role does not serve HTTP")
	}
}
