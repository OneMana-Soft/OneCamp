//go:build integration
// +build integration

package integration_test

// An update keeps everyone signed in. The release before this one ran Redis
// with the image's own command: snapshots (dump.rdb) on the host, written as
// Redis stops, and no append-only file. The update starts Redis with the
// shipped compose file's command over that same /data, runs the Makefile's
// clean-up (REDIS_KEEP, which keeps the sign-ins and drops every cache), and
// starts the rest; a later restart still has the sign-ins.
//
// WHY. Started with --appendonly yes over a /data that holds only a snapshot,
// Redis starts EMPTY: it makes a new append-only file and never reads
// dump.rdb (Redis 7 and 8.6.2). A compose command that turned it on would
// have signed everyone out on the update that shipped it, and every guard
// that reads the files would still have passed. So this runs the image and
// the command the compose file names, and the Lua the Makefile runs, on one
// Docker volume standing in for ./data/redis_data/data.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAnUpdateKeepsEveryoneSignedIn -v

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/client"
	goredis "github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestAnUpdateKeepsEveryoneSignedIn(t *testing.T) {
	image, command := composeRedisCommand(t)
	keep := makefileVariable(t, "REDIS_KEEP")
	password := "test-" + uuid.NewString()
	for i, arg := range command {
		command[i] = strings.ReplaceAll(arg, "${REDIS_PASSWORD}", password)
	}
	volume := "onecamp-test-redis-" + uuid.NewString()[:8]
	t.Cleanup(func() { removeDockerVolume(t, volume) })
	ctx := context.Background()
	signIns := map[string]string{"auth:refresh:alice": "a session", "auth:refresh-prev:alice": "the one before"}

	// The release before: the image's own command.
	before := startRedisOn(t, image, nil, volume, "")
	for k, v := range signIns {
		if err := before.db.Set(ctx, k, v, 0).Err(); err != nil {
			t.Fatal(err)
		}
	}
	if err := before.db.Set(ctx, "profile:alice", "a cached profile", 0).Err(); err != nil {
		t.Fatal(err)
	}
	before.stop(t) // compose down: SIGTERM, and Redis writes its snapshot as it stops

	// The update: the compose file's command over the same data, then the
	// clean-up, as build_restart_all runs it with only Redis started.
	now := startRedisOn(t, image, command, volume, password)
	dropped, err := now.db.Eval(ctx, keep, nil).Int()
	if err != nil {
		t.Fatalf("the clean-up: %v", err)
	}
	if dropped != 1 {
		t.Errorf("the clean-up dropped %d keys, want the 1 cache", dropped)
	}
	now.has(t, "after the update", signIns)
	if n, _ := now.db.Exists(ctx, "profile:alice").Result(); n != 0 {
		t.Error("a cache the release before wrote survived the update")
	}

	// A later restart of this release.
	now.stop(t)
	again := startRedisOn(t, image, command, volume, password)
	again.has(t, "after a restart", signIns)
}

type redisOnVolume struct {
	c  testcontainers.Container
	db *goredis.Client
}

// startRedisOn starts image with command (the image's own when nil) and /data
// on volume, and connects to it with password.
func startRedisOn(t *testing.T, image string, command []string, volume, password string) *redisOnVolume {
	t.Helper()
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        image,
			Cmd:          command,
			Env:          map[string]string{"REDIS_PASSWORD": password, "REDISCLI_AUTH": password},
			ExposedPorts: []string{"6379/tcp"},
			Mounts:       testcontainers.ContainerMounts{testcontainers.VolumeMount(volume, "/data")},
			WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start %s: %v", image, err)
	}
	r := &redisOnVolume{c: c}
	t.Cleanup(func() {
		if r.db != nil {
			_ = r.db.Close()
		}
		_ = c.Terminate(context.Background())
	})
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := c.MappedPort(ctx, "6379")
	if err != nil {
		t.Fatal(err)
	}
	r.db = goredis.NewClient(&goredis.Options{Addr: fmt.Sprintf("%s:%s", host, port.Port()), Password: password})
	if err := r.db.Ping(ctx).Err(); err != nil {
		t.Fatalf("reach %s: %v", image, err)
	}
	return r
}

// stop stops Redis as compose does (SIGTERM, ten seconds before a kill) and
// removes the container, leaving the volume.
func (r *redisOnVolume) stop(t *testing.T) {
	t.Helper()
	_ = r.db.Close()
	r.db = nil
	grace := 10 * time.Second
	if err := r.c.Stop(context.Background(), &grace); err != nil {
		t.Fatalf("stop redis: %v", err)
	}
	if err := r.c.Terminate(context.Background()); err != nil {
		t.Fatalf("remove redis: %v", err)
	}
}

func (r *redisOnVolume) has(t *testing.T, when string, want map[string]string) {
	t.Helper()
	for k, v := range want {
		got, err := r.db.Get(context.Background(), k).Result()
		if errors.Is(err, goredis.Nil) {
			t.Errorf("%s %s is gone: whoever it signed in is signed out", when, k)
			continue
		}
		if err != nil || got != v {
			t.Errorf("%s %s = %q (%v), want %q", when, k, got, err, v)
		}
	}
}

// composeRedisCommand is the redis service's image and command in the
// shipped compose file.
func composeRedisCommand(t *testing.T) (image string, command []string) {
	t.Helper()
	raw, err := os.ReadFile("../../distribute-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	in := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") && strings.HasSuffix(strings.TrimSpace(line), ":") {
			in = strings.TrimSpace(line) == "redis:"
			continue
		}
		if !in {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(trimmed, "image:"); ok {
			image = strings.TrimSpace(rest)
		}
		if rest, ok := strings.CutPrefix(trimmed, "command:"); ok {
			if err := json.Unmarshal([]byte(strings.TrimSpace(rest)), &command); err != nil {
				t.Fatalf("redis's command isn't a list: %s", line)
			}
		}
	}
	if image == "" || len(command) == 0 {
		t.Fatalf("no image or command for redis in distribute-compose.yml (image %q, command %q)", image, command)
	}
	return image, command
}

// makefileVariable is a one-line variable of the shipped Makefile.
func makefileVariable(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("../../Makefile-distribute")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + ` = (.+)$`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatalf("Makefile-distribute defines no %s", name)
	}
	return m[1]
}

// removeDockerVolume removes the test's own volume.
func removeDockerVolume(t *testing.T, name string) {
	ctx := context.Background()
	cli, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		t.Logf("docker client: %v; volume %s is left", err, name)
		return
	}
	defer cli.Close()
	if _, err := cli.VolumeRemove(ctx, name, client.VolumeRemoveOptions{Force: true}); err != nil {
		t.Logf("remove volume %s: %v", name, err)
	}
}
