package helpers

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// No secret is printed by, or rides on the command line of, the make targets
// that hand credentials to a container or a hashing tool.
//
// An argument is in ps for every account on the machine while the process
// runs, and a docker argument is also kept in the daemon's record of the
// container. A value printed by a target lands in whatever keeps the output: a
// provisioning log, a CI job, a paste into a support email. Found on a test
// server, 16 Oct 2026: migrate_up printed the LiveKit API secret through sqlx's
// .env warning (TestMigrateUpGivesSqlxOnlyTheMigrations), and these targets
// had the same shapes: storage-attach and storage-detach sourced .env, so the
// shell ran LIVEKIT_API_KEY's second word as a command and printed it in a
// "not found" error, and put both MinIO credentials on docker's command line
// for as long as a copy ran; journey did the same with its API token; and
// bcrypt_hash handed the password to htpasswd as an argument.

// installSecrets are the values written into the scratch install's .env, each
// one distinct so a leak names its source.
var installSecrets = map[string]string{
	"MINIO_PASSWORD":          "minio-root-pw-5c1e",
	"MINIO_SECRET_ACCESS_KEY": "minio-api-secret-9b7d",
	"DB_PASSWORD":             "db-pw-4a0f",
	"LIVEKIT_API_PASS":        "lk-secret-e2c8",
}

// scratchInstallWithSecrets is scratchInstall with an .env holding
// installSecrets, LIVEKIT_API_KEY in its "name: secret" form, and a stand-in
// docker on PATH that answers what these targets ask of the stack and records
// each call's arguments and the credentials it was given by name.
func scratchInstallWithSecrets(t *testing.T) (dir, log string, env []string) {
	t.Helper()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed")
	}
	dir = scratchInstall(t)
	body := "DB_USER=onecamp\nDB_NAME=onecamp\nMINIO_USER=root\nMINIO_HOST=onecamp-minio.example.com\n" +
		"MINIO_SSL=true\nMINIO_ACCESS_KEY_ID=apikey\nUSER_UPLOAD_BUCKET_NAME=onecamp-uploads\n" +
		"LIVEKIT_API_KEY_NAME=onecamp\nLIVEKIT_API_KEY=onecamp: " + installSecrets["LIVEKIT_API_PASS"] + "\n"
	for k, v := range installSecrets {
		body += k + "=" + v + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	log = filepath.Join(bin, "calls.log")
	fake := `#!/bin/sh
{ printf 'CALL'; for a in "$@"; do printf ' [%s]' "$a"; done; printf '\n'
  printf 'ENV MC_HOST_local=%s MC_HOST_remote=%s ONECAMP_JOURNEY_TOKEN=%s\n' "$MC_HOST_local" "$MC_HOST_remote" "$ONECAMP_JOURNEY_TOKEN"; } >> "` + log + `"
case " $* " in *" ps "*) echo cid0123;; esac
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	// A disk with room to spare, whatever this machine's is: storage-detach
	// refuses to copy files back onto a disk they would fill.
	roomy := `#!/bin/sh
printf 'Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/x 1000000000 1000 999999000 1%% /\n'
`
	if err := os.WriteFile(filepath.Join(bin, "df"), []byte(roomy), 0o755); err != nil {
		t.Fatal(err)
	}
	env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir, log, env
}

// runTarget runs a make target in dir and fails the test on an error.
func runTarget(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("make", append([]string{"--no-print-directory"}, args...)...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// argvLines are the recorded calls' argument lines, without their ENV lines.
func argvLines(t *testing.T, log string) []string {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("docker was never called: %v", err)
	}
	var calls []string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(l, "CALL") {
			calls = append(calls, l)
		}
	}
	return calls
}

func TestStorageAttachAndDetachKeepCredentialsOffCommandLines(t *testing.T) {
	dir, log, env := scratchInstallWithSecrets(t)
	const storageSecret = "remote-bucket-secret-77aa"
	attachEnv := append(env, "STORAGE_ENDPOINT=s3.example.net", "STORAGE_BUCKET=team-files",
		"STORAGE_ACCESS=remote-access", "STORAGE_SECRET="+storageSecret)
	out := runTarget(t, dir, attachEnv, "storage-attach")
	out += runTarget(t, dir, env, "storage-detach")
	secrets := []string{storageSecret}
	for _, v := range installSecrets {
		secrets = append(secrets, v)
	}
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("storage-attach/detach printed a secret:\n%s", out)
		}
		for _, call := range argvLines(t, log) {
			if strings.Contains(call, s) {
				t.Errorf("a secret is on docker's command line: %s", call)
			}
		}
	}
	raw, _ := os.ReadFile(log)
	if !strings.Contains(string(raw), "MC_HOST_remote=https://remote-access:"+storageSecret+"@s3.example.net") {
		t.Errorf("mc was not given the bucket's credentials by name:\n%s", raw)
	}
	if !strings.Contains(string(raw), "MC_HOST_local=http://root:"+installSecrets["MINIO_PASSWORD"]+"@minio:9000") {
		t.Errorf("mc was not given this machine's MinIO credentials by name:\n%s", raw)
	}
	if !strings.Contains(out, "ok    attached") || !strings.Contains(out, "ok    detached") {
		t.Errorf("attach and detach did not finish:\n%s", out)
	}
}

func TestJourneyPassesItsTokenByName(t *testing.T) {
	dir, log, env := scratchInstallWithSecrets(t)
	const token = "oc_journey-token-31f0"
	out := runTarget(t, dir, append(env, "ONECAMP_JOURNEY_TOKEN="+token, "ONECAMP_JOURNEY_PROJECT=p1"), "journey")
	if strings.Contains(out, token) {
		t.Errorf("make journey printed its token:\n%s", out)
	}
	for _, call := range argvLines(t, log) {
		if strings.Contains(call, token) {
			t.Errorf("the journey token is on docker's command line: %s", call)
		}
	}
	raw, _ := os.ReadFile(log)
	if !strings.Contains(string(raw), "ONECAMP_JOURNEY_TOKEN="+token) {
		t.Errorf("the token did not reach the container by name:\n%s", raw)
	}
}

// create-traefik-password (and reset-password, through the same bcrypt_hash)
// hand htpasswd the password on stdin.
func TestBcryptHashReadsThePasswordOnStdin(t *testing.T) {
	dir, _, env := scratchInstallWithSecrets(t)
	bin := t.TempDir()
	argvLog, stdinLog := filepath.Join(bin, "argv"), filepath.Join(bin, "stdin")
	fake := `#!/bin/sh
printf '%s\n' "$*" > "` + argvLog + `"
cat > "` + stdinLog + `"
echo 'x:$2y$10$abcdefghijklmnopqrstuuabcdefghijklmnopqrstuvwxyz01234'
`
	if err := os.WriteFile(filepath.Join(bin, "htpasswd"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	const pw = "traefik-dash-pw-6e2b"
	env = append(env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out := runTarget(t, dir, env, "create-traefik-password", "PASSWORD="+pw)
	argv, _ := os.ReadFile(argvLog)
	stdin, _ := os.ReadFile(stdinLog)
	if strings.Contains(string(argv), pw) || strings.Contains(out, pw) {
		t.Errorf("the password is on htpasswd's command line or in the output: argv %q", argv)
	}
	if strings.TrimSpace(string(stdin)) != pw {
		t.Errorf("htpasswd did not get the password on stdin (got %q)", stdin)
	}
	b, _ := os.ReadFile(filepath.Join(dir, ".env"))
	if !strings.Contains(string(b), "TRAEFIK_HASHED_PASSWORD=$$2y$$10$$") {
		t.Errorf("TRAEFIK_HASHED_PASSWORD was not written from the hash:\n%s", b)
	}
}

// install hands install_run the Traefik password in the environment, and no
// target passes a password to a sub-make on its command line, where ps shows
// it for the whole of an install.
func TestNoPasswordOnASubMakeCommandLine(t *testing.T) {
	raw, err := os.ReadFile("../Makefile-distribute")
	if err != nil {
		t.Fatal(err)
	}
	subMakePassword := regexp.MustCompile(`\$\(MAKE\).*\bPASSWORD=`)
	for i, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if subMakePassword.MatchString(line) {
			t.Errorf("Makefile-distribute:%d passes a password on a sub-make's command line: %s", i+1, strings.TrimSpace(line))
		}
	}
	if !strings.Contains(string(raw), `OC_PW="$$pw" OC_GEN="$$gen"`) || !strings.Contains(string(raw), `PASSWORD="$${OC_PW:-$$PASSWORD}" $(MAKE)`) {
		t.Error("install no longer hands the Traefik password to install_run in the environment")
	}
}

// A recipe in either Makefile, the shipped one or the dev one the demo deploys
// with, may not mount the whole checkout or install directory into a container
// (.env is in it), and may not put a credential on a command line: a secret
// variable expanded by make into recipe text (make echoes unprefixed lines,
// and the shell's own command line holds the text while it runs), a
// credential handed to docker by value (-e NAME=value) instead of by name, a
// database URL as an argument, htpasswd's -b, or .env run as a shell script.
func TestNoRecipeMountsTheCheckoutOrPutsASecretOnACommandLine(t *testing.T) {
	rules := []struct {
		why string
		re  *regexp.Regexp
	}{
		{"mounts the whole directory, .env included", regexp.MustCompile(`-v\s+"?(\$\(shell pwd\)|\$\$\(pwd\)|\$\(CURDIR\)|\$\$PWD|\.)"?:`)},
		{"expands a secret into the recipe's command line", regexp.MustCompile(`(^|[^$])\$[({]([A-Z0-9_]*_)?(PASSWORD|SECRET|PASS|TOKEN|KEK|KEY)[)}]`)},
		{"hands docker a credential by value, not by name", regexp.MustCompile(`-e\s+"?([A-Za-z0-9_]*(PASSWORD|SECRET|PASS|TOKEN|KEK|KEY)|MC_HOST_[A-Za-z0-9_]+)=`)},
		{"gives sqlx the database URL as an argument", regexp.MustCompile(`--database-url`)},
		{"gives htpasswd the password as an argument (-b)", regexp.MustCompile(`htpasswd\s+-[A-Za-z]*b`)},
		{"runs .env as a shell script", regexp.MustCompile(`(^|[;&|({]\s*)(\.|source)\s+(\./)?\.env\b`)},
	}
	for _, file := range []string{"../Makefile-distribute", "../Makefile"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		inDefine := false
		for i, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, "define "):
				inDefine = true
				continue
			case trimmed == "endef":
				inDefine = false
				continue
			}
			recipe := strings.HasPrefix(line, "\t") || inDefine
			if !recipe || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "@#") {
				continue
			}
			for _, r := range rules {
				if r.re.MatchString(line) {
					t.Errorf("%s:%d %s: %s", strings.TrimPrefix(file, "../"), i+1, r.why, trimmed)
				}
			}
		}
	}
}
