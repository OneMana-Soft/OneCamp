package helpers

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Redis takes the password every client already sends.
//
// WHAT WENT WRONG. The official image ignores REDIS_PASSWORD, so Redis ran
// with no password at all, open to every container on its network, while the
// API, collab, LiveKit and egress each sent the .env value.
func TestRedisRequiresThePasswordItsClientsSend(t *testing.T) {
	raw, err := os.ReadFile("../distribute-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	services := map[string]string{}
	var name string
	for _, line := range strings.Split(string(raw), "\n") {
		if m := mountServiceKey.FindStringSubmatch(line); m != nil {
			name = m[1]
			continue
		}
		if name != "" {
			services[name] += line + "\n"
		}
	}
	redis := services["redis"]
	if !strings.Contains(redis, `command: ["redis-server", "--requirepass", "${REDIS_PASSWORD}"`) {
		t.Errorf("redis isn't started with --requirepass from REDIS_PASSWORD:\n%s", redis)
	}
	if !strings.Contains(redis, "REDISCLI_AUTH=${REDIS_PASSWORD}") {
		t.Error("redis-cli inside the container (the health check, make's clean-up) can't sign in: no REDISCLI_AUTH")
	}
	// Redis keeps the snapshots (dump.rdb) every install already has. Turned
	// on over a /data with only a snapshot, the append-only file starts Redis
	// empty and dump.rdb is never read, so that update would sign everyone
	// out: the sign-ins redis-keep-sign-ins exists to keep.
	if regexp.MustCompile(`(?m)^\s*command:.*appendonly`).MatchString(redis) {
		t.Errorf("redis turns on the append-only file, which starts an existing install's Redis empty:\n%s", redis)
	}
	// Every service that reaches Redis sends the same password.
	clients := 0
	for svc, block := range services {
		if svc == "redis" || !strings.Contains(block, "redis:6379") && !strings.Contains(block, "REDIS_HOST") && !strings.Contains(block, "- .env") {
			continue
		}
		clients++
		if !strings.Contains(block, "REDIS_PASSWORD") && !strings.Contains(block, "- .env") {
			t.Errorf("%s reaches Redis without REDIS_PASSWORD, so it would be refused", svc)
		}
	}
	if clients < 3 {
		t.Fatalf("found %d Redis clients; the pattern no longer matches the compose file", clients)
	}
}

// make secrets generates the API's storage access key and an empty Redis
// password, prints neither, and leaves values an install already has.
func TestSecretsMakesTheMissingKeysAndKeepsExistingOnes(t *testing.T) {
	dir := scratchInstall(t)
	env := filepath.Join(dir, ".env")
	write := func(body string) {
		if err := os.WriteFile(env, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := func() map[string]string {
		b, err := os.ReadFile(env)
		if err != nil {
			t.Fatal(err)
		}
		kv := map[string]string{}
		for _, l := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(l, "="); ok {
				kv[k] = v
			}
		}
		return kv
	}

	write("MINIO_ACCESS_KEY_ID=__CHANGE_ME_MINIO_ACCESS_KEY__\nREDIS_PASSWORD=\n")
	out, err := runMake(t, dir, os.Environ(), "secrets")
	if err != nil {
		t.Fatalf("make secrets: %v\n%s", err, out)
	}
	kv := read()
	if !regexp.MustCompile(`^[A-Za-z0-9]{20}$`).MatchString(kv["MINIO_ACCESS_KEY_ID"]) {
		t.Errorf("MINIO_ACCESS_KEY_ID is %q; want 20 generated characters", kv["MINIO_ACCESS_KEY_ID"])
	}
	if !regexp.MustCompile(`^[A-Za-z0-9]{40}$`).MatchString(kv["REDIS_PASSWORD"]) {
		t.Errorf("an empty REDIS_PASSWORD became %q; want 40 generated characters", kv["REDIS_PASSWORD"])
	}
	for _, v := range []string{kv["MINIO_ACCESS_KEY_ID"], kv["REDIS_PASSWORD"]} {
		if v != "" && strings.Contains(out, v) {
			t.Error("make secrets printed a generated value")
		}
	}

	// An install that already has values keeps them: minio-init leaves the
	// existing key alone, and a password in use must not change underneath it.
	write("MINIO_ACCESS_KEY_ID=dfasd\nREDIS_PASSWORD=already-set\n")
	if out, err := runMake(t, dir, os.Environ(), "secrets"); err != nil {
		t.Fatalf("make secrets: %v\n%s", err, out)
	}
	if kv := read(); kv["MINIO_ACCESS_KEY_ID"] != "dfasd" || kv["REDIS_PASSWORD"] != "already-set" {
		t.Errorf("make secrets changed values an install already had: %v", kv)
	}
}

// The template names no fixed storage key: every install generates its own.
func TestTheTemplateNamesNoFixedStorageKey(t *testing.T) {
	raw, err := os.ReadFile("../vars/.env.prod")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^MINIO_ACCESS_KEY_ID=__[A-Za-z0-9_]+__$`).Match(raw) {
		t.Error("vars/.env.prod sets MINIO_ACCESS_KEY_ID to a fixed value; every install would share it")
	}
}
