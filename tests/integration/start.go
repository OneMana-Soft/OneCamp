//go:build integration
// +build integration

package integration

// Every container the harness starts, it starts here.
//
// WHY. One `go test` over several packages runs one binary per package, and
// testcontainers gives them all one session (the session id comes from the
// `go test` process), so they share one reaper container: the first binary to
// start a container starts it, and the others reuse it. When the binaries
// using it have all gone, the reaper exits and Docker removes it. A binary
// that starts its first container in those few seconds finds the session's
// reaper exited or "removing", and testcontainers-go v0.42.0 gives up at once
// (retries: 0) instead of starting a new one:
//
//	start postgres container: create container: reaper: from container
//	"9195b86b": wait for reaper 9195b86b: detect internal port: check target:
//	retries: 0, last err: unexpected container status "removing"
//
// Four integrator gates went RED on it on 9 and 10 Nov 2026 (b261, b262, b263,
// b266, V-856), each the gate's only FAIL and each a pass when run again by
// name: TestEveryForeignKeyHasAnIndex three times, business/Import's
// TestImportedFields once. A container start is now tried again, a few times
// a few seconds apart, when it failed on exactly that: the session's reaper
// was there but not running. By the next try it is gone, and testcontainers
// starts a new one. Any other failure (an image that won't pull, a server
// that never gets ready, a reaper that can't be started) fails at once with
// its own text, as before.
//
// helpers/integrationHarnessGuard_test.go fails a call to
// testcontainers.GenericContainer anywhere else in the module.

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

// reaperWaits are the pauses before each try after the first: five tries,
// about 15 s in all. Docker removes an exited reaper in well under a second
// on a quiet machine and a few seconds on a busy one.
var reaperWaits = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}

// reaperRetries counts the tries made again because the reaper was going, so
// a test can tell that it met the race rather than missed it.
var reaperRetries atomic.Int64

// StartContainer starts a container as testcontainers.GenericContainer does,
// trying again while the session's reaper is going (reaperGoing). A start
// that fails leaves no container behind.
func StartContainer(ctx context.Context, req testcontainers.GenericContainerRequest) (testcontainers.Container, error) {
	return startTrying(ctx, req, testcontainers.GenericContainer, reaperWaits)
}

// startTrying is StartContainer with the start and the pauses given.
func startTrying(ctx context.Context, req testcontainers.GenericContainerRequest,
	start func(context.Context, testcontainers.GenericContainerRequest) (testcontainers.Container, error),
	waits []time.Duration,
) (testcontainers.Container, error) {
	for try := 0; ; try++ {
		c, err := start(ctx, req)
		if err == nil {
			return c, nil
		}
		// testcontainers connects a container to the reaper after making
		// it, so a start that failed there leaves a made container that no
		// reaper will remove. No caller uses the container of a failed start.
		_ = testcontainers.TerminateContainer(c)
		if try == len(waits) || !reaperGoing(err) {
			return nil, err
		}
		reaperRetries.Add(1)
		fmt.Printf("integration: %s: the session's reaper was going (try %d of %d), trying again in %s: %v\n",
			req.Image, try+1, len(waits)+1, waits[try], err)
		select {
		case <-time.After(waits[try]):
		case <-ctx.Done():
			return nil, err
		}
	}
}

// reaperGoing is whether err is testcontainers finding the session's reaper
// container there but not running (exited, "removing", or not yet started),
// when it went to reuse it. Only that path writes "wait for reaper"
// (reaperSpawner.fromContainer), and "check target" is its wait reading a
// state other than running (wait.checkState). A reaper that fails to start,
// and every failure of the container itself, reads otherwise.
func reaperGoing(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "reaper: ") &&
		strings.Contains(msg, "wait for reaper ") &&
		strings.Contains(msg, "check target: ")
}
