package helpers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeReadyDocker stands in for docker: the `compose ... exec ... curl` that
// api-ready runs prints $READY_ANSWER, as the API's /health/ready would.
const fakeReadyDocker = `#!/bin/sh
case " $* " in
*/health/ready*) printf '%b' "$READY_ANSWER" ;;
esac
exit 0
`

// api-ready prints the API's answer and fails on any FAIL line. An answer in
// which every check fails is still an answer, and it names what is down. It
// used to be taken for no answer at all: with every service down, verify and
// wait-ready said only "no readiness answer" and hid which ones.
func TestApiReadyNamesTheChecksThatFail(t *testing.T) {
	dir := scratchInstall(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fakeReadyDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, answer string
		ready        bool
		want, not    []string
	}{
		{"every check passes", `ok    database\nok    realtime\n`, true,
			[]string{"ok    database", "ok    realtime"}, []string{"FAIL"}},
		{"one check fails", `ok    database\nFAIL  realtime: not connected to the broker\n`, false,
			[]string{"ok    database", "FAIL  realtime: not connected to the broker"}, []string{"no readiness answer"}},
		{"every check fails", `FAIL  database: connection refused\nFAIL  realtime: not connected to the broker\n`, false,
			[]string{"FAIL  database: connection refused", "FAIL  realtime: not connected to the broker"}, []string{"no readiness answer"}},
		{"no answer", ``, false, []string{"FAIL  api: no readiness answer"}, nil},
		{"an answer that isn't a readiness answer", `404 page not found\n`, false, []string{"FAIL  api: no readiness answer"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "READY_ANSWER="+c.answer)
			out, err := runMake(t, dir, env, "api-ready")
			if (err == nil) != c.ready {
				t.Errorf("api-ready succeeded=%v; want %v\n%s", err == nil, c.ready, out)
			}
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Errorf("api-ready doesn't say %q:\n%s", w, out)
				}
			}
			for _, n := range c.not {
				if strings.Contains(out, n) {
					t.Errorf("api-ready says %q:\n%s", n, out)
				}
			}
		})
	}
}

// verify and wait-ready have one answer to "is it ready": the running API's
// own dependency checks (make api-ready, helpers.ServeReadiness).
//
// WHAT WENT WRONG. verify grepped the API's startup log for "Successfully
// connected" lines while wait-ready looked at containers. After an update on a
// busy machine the API was healthy (so wait-ready passed) but had never
// reached the broker, and verify said "FAIL mqtt"; and once the log had moved
// on, a healthy install read FAIL for everything. Seen on a test server.
func TestVerifyAndWaitReadyShareOneReadiness(t *testing.T) {
	raw, err := os.ReadFile("../Makefile-distribute")
	if err != nil {
		t.Fatal(err)
	}
	mk := string(raw)
	ready := recipeOf(t, mk, "api-ready")
	if !strings.Contains(ready, "exec -T go-service curl") || !strings.Contains(ready, "/health/ready") {
		t.Errorf("api-ready doesn't ask the API's /health/ready:\n%s", ready)
	}
	verify := recipeOf(t, mk, "verify")
	if !strings.Contains(verify, "--no-print-directory api-ready") {
		t.Error("verify doesn't report the backing services through make api-ready")
	}
	if strings.Contains(verify, "Successfully connected") {
		t.Error("verify still judges the backing services from the API's startup log")
	}
	if !strings.Contains(recipeOf(t, mk, "wait-ready"), "--no-print-directory api-ready") {
		t.Error("wait-ready doesn't wait for make api-ready, so it can call ready what verify calls FAIL")
	}
}

// The broker has a health check, so wait-ready (which waits for every health
// check) waits for it, and Traefik routes it only once it answers.
func TestTheBrokerHasAHealthCheck(t *testing.T) {
	raw, err := os.ReadFile("../distribute-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	var block []string
	in := false
	for _, line := range strings.Split(string(raw), "\n") {
		if m := mountServiceKey.FindStringSubmatch(line); m != nil {
			in = m[1] == "emqx"
			continue
		}
		if in {
			block = append(block, line)
		}
	}
	emqx := strings.Join(block, "\n")
	if !strings.Contains(emqx, "healthcheck:") || !strings.Contains(emqx, `"curl", "-sf"`) || !strings.Contains(emqx, "/status") {
		t.Errorf("emqx has no health check on its /status page:\n%s", emqx)
	}
	// `emqx ctl status` boots a second Erlang node for every check: about 5 s
	// of CPU each time (measured on emqx/emqx:6.0.0), half a core all day.
	if strings.Contains(emqx, `"ctl", "status"`) {
		t.Errorf("emqx's health check runs emqx ctl, which costs seconds of CPU every run:\n%s", emqx)
	}
}
