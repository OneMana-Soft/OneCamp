//go:build integration
// +build integration

package integration

// The container starter (start.go) against the reaper race of V-856.
// Run: go test -tags=integration ./tests/integration/ -run 'TestAContainerStartsWhileTheRunsReaperIsGoing|TestOnlyAReaperThatIsGoingIsTriedAgain' -v

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

// reaperChildEnv marks the child run of TestAContainerStartsWhileTheRunsReaperIsGoing.
const reaperChildEnv = "ONECAMP_V856_REAPER_CHILD"

// The race, on demand. A `go test` over several packages gives every binary
// one reaper; when the binaries using it have gone, it exits and Docker
// removes it. This stands in a reaper of the run's session that has exited,
// with Docker removing it a moment later, as it does a real one, and then
// starts a container, as the next package's first test does.
//
// The test runs itself again as a child process: the child's session is its
// own (testcontainers takes the session from the parent process), so its
// stand-in can't meet a real reaper of this run, and the child has started
// no container yet, so it goes to look for the session's reaper.
func TestAContainerStartsWhileTheRunsReaperIsGoing(t *testing.T) {
	if os.Getenv(reaperChildEnv) == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestAContainerStartsWhileTheRunsReaperIsGoing$", "-test.v", "-test.count=1", "-test.timeout=3m")
		cmd.Env = append(os.Environ(), reaperChildEnv+"=1")
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "--- PASS: TestAContainerStartsWhileTheRunsReaperIsGoing") {
			t.Fatalf("the child run (a container started while its session's reaper was going) failed: %v\n%s", err, out)
		}
		t.Logf("the child run:\n%s", out)
		return
	}

	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	sid := testcontainers.SessionID()
	name := "reaper_" + sid
	// The name, labels and image testcontainers looks a session's reaper up
	// by (v0.42.0's reaper image: any other image is read as an ordinary
	// container, and reading one connects it to the reaper being looked up,
	// which waits on itself). Without the Docker socket Ryuk exits at once.
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	docker("run", "-d", "--name", name,
		"--label", "org.testcontainers.sessionId="+sid,
		"--label", "org.testcontainers.reaper=true",
		"--label", "org.testcontainers.ryuk=true",
		"--expose", "8080", "testcontainers/ryuk:0.13.0")
	docker("wait", name)
	if state := docker("inspect", "-f", "{{.State.Status}}", name); state != "exited" {
		t.Fatalf("the stand-in reaper is %q, not exited", state)
	}
	// Docker removes it once the start has met it: right after the first
	// try fails, as a real reaper goes in the seconds after its last binary.
	go func() {
		for deadline := time.Now().Add(time.Minute); reaperRetries.Load() == 0 && time.Now().Before(deadline); {
			time.Sleep(20 * time.Millisecond)
		}
		_ = exec.Command("docker", "rm", "-f", name).Run()
	}()

	began := time.Now()
	c, err := StartContainer(context.Background(), testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{Image: "alpine:3.22", Cmd: []string{"sleep", "300"}},
		Started:          true,
	})
	if err != nil {
		t.Fatalf("a container started while the session's reaper was going failed: %v", err)
	}
	defer terminate(c)
	if n := reaperRetries.Load(); n == 0 {
		t.Fatal("the start never met the going reaper, so this proved nothing: the stand-in was not looked up")
	}
	t.Logf("started after %d tries again in %s", reaperRetries.Load(), time.Since(began).Round(time.Millisecond))
}

// What the four gates met, word for word (b261, b262, b263, b266), and the
// same reaper exited, which is the step before Docker removes it.
var reaperGoingErrors = []string{
	`start postgres container: create container: reaper: from container "b2170efc": wait for reaper b2170efc: detect internal port: check target: retries: 0, last err: unexpected container status "removing"`,
	`start postgres container: create container: reaper: from container "9195b86b": wait for reaper 9195b86b: detect internal port: check target: retries: 0, last err: unexpected container status "removing"`,
	`start postgres container: create container: reaper: from container "3be90bb9": wait for reaper 3be90bb9: detect internal port: check target: retries: 0, last err: unexpected container status "removing"`,
	`start postgres container: create container: reaper: from container "680ad816": wait for reaper 680ad816: detect internal port: check target: retries: 0, last err: unexpected container status "removing"`,
	`create container: reaper: from container "1a2b3c4d": wait for reaper 1a2b3c4d: detect internal port: check target: retries: 0, last err: container exited with code 0`,
	`create container: reaper: from container "1a2b3c4d": wait for reaper 1a2b3c4d: mapped port: check target: retries: 1, port: "8080/tcp", last err: unexpected container status "removing"`,
}

// Real failures, which a second try would not mend.
var realStartErrors = []string{
	// The reaper itself can't start.
	`create container: reaper: new reaper: run container: create container: pull image: Error response from daemon: pull access denied for testcontainers/ryuk`,
	// Docker can't be asked where the reaper is.
	`create container: reaper: look up container: container list: Cannot connect to the Docker daemon at unix:///var/run/docker.sock`,
	// The image won't pull.
	`create container: pull image: Error response from daemon: manifest for postgres:99 not found`,
	// The container itself stops while its own wait reads it.
	`start container: started hook: wait until ready: check target: retries: 3 address: localhost:5432: container exited with code 1`,
	// The server never gets ready.
	`start container: started hook: wait until ready: context deadline exceeded`,
}

// A start is tried again only while the session's reaper is going, a bounded
// number of times; any other failure comes back at once with its own text.
func TestOnlyAReaperThatIsGoingIsTriedAgain(t *testing.T) {
	for _, s := range reaperGoingErrors {
		if !reaperGoing(errors.New(s)) {
			t.Errorf("not read as the reaper going: %s", s)
		}
	}
	for _, s := range realStartErrors {
		if reaperGoing(errors.New(s)) {
			t.Errorf("read as the reaper going, so it would be tried again: %s", s)
		}
	}

	req := testcontainers.GenericContainerRequest{ContainerRequest: testcontainers.ContainerRequest{Image: "postgres:12-alpine"}}
	fake := func(errs ...error) (func(context.Context, testcontainers.GenericContainerRequest) (testcontainers.Container, error), *int) {
		calls := 0
		return func(context.Context, testcontainers.GenericContainerRequest) (testcontainers.Container, error) {
			calls++
			if calls <= len(errs) {
				return nil, errs[calls-1]
			}
			return &testcontainers.DockerContainer{}, nil
		}, &calls
	}
	going := errors.New(reaperGoingErrors[1])
	short := []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}

	t.Run("a reaper going twice, then a start", func(t *testing.T) {
		start, calls := fake(going, going)
		if c, err := startTrying(context.Background(), req, start, short); err != nil || c == nil {
			t.Fatalf("got %v, %v; want the container the third try started", c, err)
		}
		if *calls != 3 {
			t.Fatalf("%d tries, want 3", *calls)
		}
	})

	t.Run("a reaper that never goes stops after the last try", func(t *testing.T) {
		start, calls := fake(going, going, going, going, going, going, going)
		_, err := startTrying(context.Background(), req, start, short)
		if !errors.Is(err, going) {
			t.Fatalf("got %v; want the reaper's own error", err)
		}
		if *calls != len(short)+1 {
			t.Fatalf("%d tries, want %d", *calls, len(short)+1)
		}
	})

	t.Run("a real failure fails at once with its own text", func(t *testing.T) {
		for _, s := range realStartErrors {
			real := errors.New(s)
			start, calls := fake(real, real)
			done := make(chan error, 1)
			// An hour's pause: a real failure tried again would never return in time.
			go func() {
				_, err := startTrying(context.Background(), req, start, []time.Duration{time.Hour})
				done <- err
			}()
			select {
			case err := <-done:
				if err != real {
					t.Errorf("got %v; want the failure itself: %s", err, s)
				}
				if *calls != 1 {
					t.Errorf("%d tries of a real failure, want 1: %s", *calls, s)
				}
			case <-time.After(5 * time.Second):
				t.Errorf("a real failure was waited on to be tried again: %s", s)
			}
		}
	})

	t.Run("the harness's own pauses are a few, a few seconds apart", func(t *testing.T) {
		var total time.Duration
		for _, w := range reaperWaits {
			total += w
		}
		if len(reaperWaits) < 2 || len(reaperWaits) > 6 || total > 30*time.Second {
			t.Fatalf("%d pauses of %s in all; want a few, under 30 s", len(reaperWaits), total)
		}
	})
}
