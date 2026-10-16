package helpers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withChecks replaces the registry with checks for the test.
func withChecks(t *testing.T, checks ...SystemCheck) {
	t.Helper()
	systemCheckMu.Lock()
	saved := systemChecks
	systemChecks = map[string]SystemCheck{}
	systemCheckMu.Unlock()
	for _, c := range checks {
		RegisterSystemCheck(c)
	}
	t.Cleanup(func() { systemCheckMu.Lock(); systemChecks = saved; systemCheckMu.Unlock() })
}

func ready(remote string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	r.RemoteAddr = remote
	w := httptest.NewRecorder()
	ServeReadiness(w, r)
	return w
}

// /health/ready answers from the running server's dependency checks, now:
// 503 and a FAIL line for any that fails, a note for one with something to
// say, and nothing about behaviour checks.
func TestReadinessReportsEachDependencyNow(t *testing.T) {
	ok := func(context.Context) error { return nil }
	withChecks(t,
		SystemCheck{Name: "database", Kind: CheckKindDependency, Probe: ok},
		SystemCheck{Name: "realtime", Kind: CheckKindDependency, Probe: func(context.Context) error {
			return errors.New("not connected to the MQTT broker")
		}},
		SystemCheck{Name: "email", Kind: CheckKindDependency, Probe: func(context.Context) error {
			return SystemCheckNote("no email key is set")
		}},
		SystemCheck{Name: "agent-queue", Kind: CheckKindBehaviour, Probe: func(context.Context) error {
			return errors.New("a behaviour check, which readiness never runs")
		}},
	)
	w := ready("127.0.0.1:51234")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d with the broker down; want 503", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"ok    database\n",
		"FAIL  realtime: not connected to the MQTT broker\n",
		"ok    email (no email key is set)\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("readiness is missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "agent-queue") {
		t.Errorf("readiness ran a behaviour check:\n%s", body)
	}

	withChecks(t, SystemCheck{Name: "database", Kind: CheckKindDependency, Probe: ok})
	if w := ready("[::1]:443"); w.Code != http.StatusOK || w.Body.String() != "ok    database\n" {
		t.Errorf("all passing: status %d, body %q; want 200 and one ok line", w.Code, w.Body.String())
	}
}

// It names which services are down, so only this machine may ask.
func TestReadinessIsOnlyForThisMachine(t *testing.T) {
	withChecks(t, SystemCheck{Name: "database", Kind: CheckKindDependency, Probe: func(context.Context) error { return nil }})
	for _, remote := range []string{"172.20.0.5:38912", "10.0.2.2:1", "203.0.113.9:443", "garbage"} {
		if w := ready(remote); w.Code != http.StatusNotFound {
			t.Errorf("a request from %s got %d; want 404", remote, w.Code)
		}
	}
}

// With nothing registered, nothing says the server is ready.
func TestReadinessWithNoChecksIsNotReady(t *testing.T) {
	withChecks(t)
	if w := ready("127.0.0.1:1"); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "FAIL") {
		t.Errorf("no checks: status %d, body %q", w.Code, w.Body.String())
	}
}
