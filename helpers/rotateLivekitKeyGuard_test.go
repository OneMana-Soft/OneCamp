package helpers

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// make rotate-livekit-key gives LiveKit a new API secret without showing it to
// anyone, and recreates exactly the services that read it.
//
// WHY IT EXISTS. Until 16 Oct 2026 every install and update printed the LiveKit
// API secret in a warning on the terminal (TestMigrateUpGivesSqlxOnlyTheMigrations),
// so a saved or shared output of one holds the key that signs every call token.
// The notes tell those admins to run this target, so it must change both lines
// LiveKit and the API read, keep a backup, never print or pass the value on a
// command line, recreate livekit, egress and the API (and the worker and the
// transcription agent only where their profile is on), and put everything back
// when the stack doesn't come back.
//
// The target runs here against a stand-in docker that answers what it and
// wait-ready ask of the stack, records every call's arguments and the LiveKit
// variables in its environment, and keeps a copy of .env as each recreate saw it.

const (
	lkOldSecret = "lk-old-secret-3f9a1c"
	lkName      = "onecamp"
)

// lkRun is one run of the target against a stand-in docker.
type lkRun struct {
	dir, calls, snaps  string
	stdout, stderr     string
	err                error
	original           []byte
	recreates          [][]string // the services named by each `up --force-recreate`
	snapshots          []string   // .env as each recreate read it
	callArgs, callEnvs []string
	awkArgs            []string // every awk call's arguments
}

func runRotateLivekitKey(t *testing.T, services, running string) lkRun {
	t.Helper()
	for _, tool := range []string{"make", "openssl", "awk"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " is not installed")
		}
	}
	r := lkRun{dir: scratchInstall(t)}
	r.original = []byte("DB_USER=onecamp\nDB_PASSWORD=db-pw-77e0\n" +
		"LIVEKIT_API_KEY_NAME=" + lkName + "\nLIVEKIT_API_PASS=" + lkOldSecret + "\n" +
		"LIVEKIT_API_KEY=" + lkName + ": " + lkOldSecret + "\nTURN_SECRET=turn-9c4d\n")
	envFile := filepath.Join(r.dir, ".env")
	if err := os.WriteFile(envFile, r.original, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(envFile, 0o640); err != nil { // past the umask
		t.Fatal(err)
	}
	bin := t.TempDir()
	r.calls = filepath.Join(bin, "calls.log")
	r.snaps = filepath.Join(bin, "snaps")
	if err := os.MkdirAll(r.snaps, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := `#!/bin/sh
{ printf 'CALL'; for a in "$@"; do printf ' [%s]' "$a"; done; printf '\n'
  printf 'ENV LIVEKIT_API_KEY=%s LIVEKIT_API_PASS=%s\n' "${LIVEKIT_API_KEY-<unset>}" "${LIVEKIT_API_PASS-<unset>}"; } >> "` + r.calls + `"
case " $* " in
  *" config --services "*) for s in ` + services + `; do echo "$s"; done ;;
  *" up "*) n=$(ls "` + r.snaps + `" | wc -l); cp .env "` + r.snaps + `/$n" ;;
  *" ps --status running --services "*) for s in ` + running + `; do echo "$s"; done ;;
  *" ps --format "*) for s in ` + services + `; do echo "$s running"; done ;;
  *" exec "*" curl "*) echo "ok   postgres"; echo "ok   redis" ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	// awk writes the new .env: a stand-in records its arguments and runs the real one.
	realAwk, _ := exec.LookPath("awk")
	awk := `#!/bin/sh
{ printf 'AWK'; for a in "$@"; do printf ' [%s]' "$a"; done; printf '\n'; } >> "` + r.calls + `"
exec "` + realAwk + `" "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "awk"), []byte(awk), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("make", "--no-print-directory", "rotate-livekit-key")
	cmd.Dir = r.dir
	// The LiveKit variables are exported here with the OLD values, as a shell
	// might hold them: compose prefers the environment to the env file, so the
	// target must not let them reach it.
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"WAIT_READY_SECONDS=9", "LIVEKIT_API_PASS="+lkOldSecret, "LIVEKIT_API_KEY="+lkName+": "+lkOldSecret)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	r.err = cmd.Run()
	r.stdout, r.stderr = stdout.String(), stderr.String()

	raw, _ := os.ReadFile(r.calls)
	for _, l := range strings.Split(string(raw), "\n") {
		if a, ok := strings.CutPrefix(l, "CALL"); ok {
			r.callArgs = append(r.callArgs, a)
			if i := strings.Index(a, " [--no-deps]"); i >= 0 && strings.Contains(a, " [up] ") {
				var svcs []string
				for _, f := range strings.Split(strings.TrimSpace(a[i+len(" [--no-deps]"):]), " ") {
					svcs = append(svcs, strings.Trim(f, "[]"))
				}
				sort.Strings(svcs)
				r.recreates = append(r.recreates, svcs)
			}
		}
		if a, ok := strings.CutPrefix(l, "AWK"); ok {
			r.awkArgs = append(r.awkArgs, a)
		}
		if e, ok := strings.CutPrefix(l, "ENV "); ok {
			r.callEnvs = append(r.callEnvs, e)
		}
	}
	for i := 0; ; i++ {
		b, err := os.ReadFile(filepath.Join(r.snaps, string(rune('0'+i))))
		if err != nil {
			break
		}
		r.snapshots = append(r.snapshots, string(b))
	}
	return r
}

// noSecretAnywhere fails the test if a secret is in the output or on a docker command line.
func (r lkRun) noSecretAnywhere(t *testing.T, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if s == "" {
			continue
		}
		if strings.Contains(r.stdout, s) || strings.Contains(r.stderr, s) {
			t.Errorf("make rotate-livekit-key printed a secret:\nstdout:\n%s\nstderr:\n%s", r.stdout, r.stderr)
		}
		for _, a := range r.callArgs {
			if strings.Contains(a, s) {
				t.Errorf("a secret is on docker's command line: %s", a)
			}
		}
		for _, a := range r.awkArgs {
			if strings.Contains(a, s) {
				t.Errorf("a secret is on awk's command line: %s", a)
			}
		}
	}
}

func TestRotateLivekitKeyChangesBothLinesAndShowsNoSecret(t *testing.T) {
	for _, tc := range []struct {
		name, services string
		want           []string
	}{
		{"an install with neither the worker nor the transcription agent",
			"postgres redis minio-init livekit egress go-service collaboration-service web",
			[]string{"egress", "go-service", "livekit"}},
		{"an install running the worker and the transcription agent",
			"postgres redis minio-init livekit egress go-service go-worker agent whisper",
			[]string{"agent", "egress", "go-service", "go-worker", "livekit"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runRotateLivekitKey(t, tc.services, tc.services)
			if r.err != nil {
				t.Fatalf("make rotate-livekit-key: %v\nstdout:\n%s\nstderr:\n%s", r.err, r.stdout, r.stderr)
			}
			if !strings.Contains(r.stdout, "LiveKit key changed; calls in progress will end") {
				t.Errorf("it doesn't say the key changed:\n%s", r.stdout)
			}
			after, err := os.ReadFile(filepath.Join(r.dir, ".env"))
			if err != nil {
				t.Fatal(err)
			}
			pass, key := envValue(string(after), "LIVEKIT_API_PASS"), envValue(string(after), "LIVEKIT_API_KEY")
			if !regexp.MustCompile(`^[A-Za-z0-9]{64}$`).MatchString(pass) {
				t.Fatalf("LIVEKIT_API_PASS is not a new 64-character secret (length %d)", len(pass))
			}
			if key != lkName+": "+pass || !livekitKeys.MatchString(key) {
				t.Errorf("LIVEKIT_API_KEY isn't the name and the new secret in LiveKit's form (LiveKit and the API would disagree)")
			}
			if strings.Contains(string(after), lkOldSecret) {
				t.Errorf("the old secret is still in .env")
			}
			for _, k := range []string{"DB_PASSWORD", "TURN_SECRET", "LIVEKIT_API_KEY_NAME"} {
				if envValue(string(after), k) != envValue(string(r.original), k) {
					t.Errorf("%s changed; only the LiveKit secret may", k)
				}
			}
			if st, err := os.Stat(filepath.Join(r.dir, ".env")); err != nil || st.Mode().Perm() != 0o640 {
				t.Errorf(".env's mode is %v, want 0640 as it was", st.Mode().Perm())
			}
			backups, _ := filepath.Glob(filepath.Join(r.dir, ".env.livekit-rotate-*"))
			if len(backups) != 1 {
				t.Fatalf("want one backup of .env, found %v", backups)
			}
			if b, _ := os.ReadFile(backups[0]); !bytes.Equal(b, r.original) {
				t.Errorf("the backup isn't the .env it replaced")
			}
			if st, _ := os.Stat(backups[0]); st.Mode().Perm() != 0o600 {
				t.Errorf("the backup's mode is %v, want 0600", st.Mode().Perm())
			}
			if left, _ := filepath.Glob(filepath.Join(r.dir, ".env.rotate.*")); len(left) != 0 {
				t.Errorf("temporary files were left beside .env: %v", left)
			}
			r.noSecretAnywhere(t, lkOldSecret, pass)
			if !strings.Contains(strings.Join(r.awkArgs, "\n"), "ENVIRON[") {
				t.Errorf("no awk call read the secret from its environment; the stand-in awk saw: %v", r.awkArgs)
			}
			if len(r.recreates) != 1 {
				t.Fatalf("want one recreate, got %d: %v", len(r.recreates), r.recreates)
			}
			if strings.Join(r.recreates[0], " ") != strings.Join(tc.want, " ") {
				t.Errorf("recreated %v, want exactly %v", r.recreates[0], tc.want)
			}
			if len(r.snapshots) != 1 || envValue(r.snapshots[0], "LIVEKIT_API_PASS") != pass {
				t.Errorf("the services were recreated before .env held the new secret")
			}
			for _, e := range r.callEnvs {
				if e != "LIVEKIT_API_KEY=<unset> LIVEKIT_API_PASS=<unset>" {
					t.Errorf("docker ran with LiveKit variables in its environment, which compose prefers to .env: %s", e)
					break
				}
			}
		})
	}
}

func TestRotateLivekitKeyPutsEverythingBackWhenTheStackDoesntComeBack(t *testing.T) {
	services := "postgres redis livekit egress go-service web"
	r := runRotateLivekitKey(t, services, "postgres redis egress go-service web") // livekit never runs
	if r.err == nil {
		t.Fatalf("make rotate-livekit-key succeeded with LiveKit down:\n%s", r.stdout)
	}
	after, _ := os.ReadFile(filepath.Join(r.dir, ".env"))
	if !bytes.Equal(after, r.original) {
		t.Errorf(".env isn't as it was after the failure")
	}
	if !strings.Contains(r.stdout, "the LiveKit key was not changed") || !strings.Contains(r.stdout, "make rotate-livekit-key again") {
		t.Errorf("it doesn't say the key is unchanged and how to retry:\n%s", r.stdout)
	}
	if len(r.recreates) != 2 || len(r.snapshots) != 2 {
		t.Fatalf("want a recreate with the new key and one with the old, got %v", r.recreates)
	}
	newPass := envValue(r.snapshots[0], "LIVEKIT_API_PASS")
	if newPass == lkOldSecret || envValue(r.snapshots[1], "LIVEKIT_API_PASS") != lkOldSecret {
		t.Errorf("the services were not recreated with the old key after the failure")
	}
	if backups, _ := filepath.Glob(filepath.Join(r.dir, ".env.livekit-rotate-*")); len(backups) != 1 {
		t.Errorf("want the backup kept, found %v", backups)
	}
	r.noSecretAnywhere(t, lkOldSecret, newPass)
}
