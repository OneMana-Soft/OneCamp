package helpers

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A fresh install must be able to store a file.
//
// WHAT WENT WRONG. Nothing ever made the bucket uploads go to or the access key the API signs in
// with. .sample.env names both (USER_UPLOAD_BUCKET_NAME, MINIO_ACCESS_KEY_ID) and MinIO starts
// knowing only its root user, so on every fresh install each upload, call recording and import
// failed with "The Access Key Id you provided does not exist in our records" until an operator made
// both by hand. make verify passed throughout: the API logs "Successfully connected with minio
// server" without asking MinIO anything.
//
// The minio-init service now makes them (other-services/minio/init.sh) once MinIO is healthy. The
// API does not wait for it: it starts without files and uses the bucket and key once they exist,
// so object storage can never hold the whole workspace down. These tests hold the wiring in place, and run the script against a stand-in
// for mc to hold its promises: an existing key is never re-made (`mc admin user add` on an existing
// user would replace its secret), no secret is ever on a command line or in the log, and nothing is
// done when files live in an attached bucket.

const minioInitScript = "../other-services/minio/init.sh"

// minio-init waits for MinIO, and nothing waits for minio-init.
//
// WHAT WENT WRONG. The API and the workers waited for minio-init to finish, and minio-init waits
// for MinIO to be healthy, so a MinIO that never got healthy, or a root password it refused, kept
// the whole workspace down. The API needs neither to start: with MinIO stopped it started and
// served everything but files, and once MinIO was back uploads worked with no restart (checked on
// a test server, 14 Oct 2026). Availability first: storage trouble stays storage trouble.
func TestNothingWaitsForMinIOInit(t *testing.T) {
	for _, file := range minioStacks {
		raw, err := os.ReadFile(file)
		if err != nil {
			if strings.Contains(file, "final-compose") && os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
		body := string(raw)
		for _, svc := range []string{"go-service", "go-worker"} {
			block := serviceLines(body, svc)
			if strings.TrimSpace(block) == "" {
				t.Fatalf("%s has no %s, so this guard watches nothing", file, svc)
			}
			if strings.Contains(block, "      minio-init:\n") {
				t.Errorf("%s: %s waits for minio-init, so a MinIO that never gets healthy keeps the whole "+
					"workspace down; it starts without files and uses the bucket and key once they exist", file, svc)
			}
		}

		init := serviceLines(body, "minio-init")
		if strings.TrimSpace(init) == "" {
			t.Errorf("%s has no minio-init service, so nothing makes the upload bucket or the API's access key", file)
			continue
		}
		for _, want := range []string{
			"      minio:\n        condition: service_healthy\n",
			"restart: \"no\"",
			"./other-services/minio/init.sh:/minio-init.sh:ro",
			"STORAGE_ATTACHED_BUCKET: ${STORAGE_ATTACHED_BUCKET:-}",
		} {
			if !strings.Contains(init, want) {
				t.Errorf("%s: minio-init is missing %q", file, want)
			}
		}
		if got, want := minioImage(t, file, init), minioImage(t, file, serviceLines(body, "minio")); got != want {
			t.Errorf("%s: minio-init runs %s but minio runs %s; it uses the mc that ships with MinIO's own image", file, got, want)
		}
	}
	if _, err := os.Stat(minioInitScript); err != nil {
		t.Errorf("minio-init mounts %s, which is missing: %v", minioInitScript, err)
	}
}

// stubMC stands in for mc. It records every command line in $WORLD/argv, every secret it is
// handed on stdin in $WORLD/stdin, and answers from files in $WORLD the way MinIO answers.
const stubMC = `#!/bin/bash
W=$WORLD
printf '%s\n' "$*" >> "$W/argv"
case "$1" in
alias)
	IFS= read -r ak; IFS= read -r sk; printf '%s\n' "$sk" >> "$W/stdin"
	n=$(cat "$W/down_for" 2>/dev/null || echo 0)
	if [ "$3" = local ] && [ "$n" -gt 0 ]; then
		echo $((n - 1)) > "$W/down_for"; echo "mc: <ERROR> Unable to initialize new alias. connection refused"; exit 1
	fi
	if [ "$3" = local ] && { [ "$ak" != "$(cat "$W/root_user")" ] || [ "$sk" != "$(cat "$W/root_pass")" ]; }; then
		echo "mc: <ERROR> Unable to initialize new alias. The request signature we calculated does not match."; exit 1
	fi
	exit 0 ;;
mb) touch "$W/bucket_${3#local/}"; exit 0 ;;
ls) [ -f "$W/bucket_${2#api/}" ]; exit ;;
esac
case "$2 $3" in
"user svcacct")
	[ -f "$W/svcacct_$6" ] && { echo "AccessKey: $6"; exit 0; }
	[ -n "${SVCACCT_ERROR:-}" ] && { echo "mc: <ERROR> $SVCACCT_ERROR"; exit 1; }
	[ -f "$W/user_$6" ] && { echo "mc: <ERROR> Unable to get information of the specified service account. Access Denied."; exit 1; }
	echo "mc: <ERROR> Unable to get information of the specified service account. The specified service account is not found (Specified service account does not exist)."; exit 1 ;;
"user info")
	[ -f "$W/user_$5" ] && { echo "AccessKey: $5"; exit 0; }
	[ -n "${USER_INFO_ERROR:-}" ] && { echo "mc: <ERROR> $USER_INFO_ERROR"; exit 1; }
	[ -f "$W/svcacct_$5" ] && { echo "mc: <ERROR> Unable to get user info. Specified IAM action is not allowed."; exit 1; }
	echo "mc: <ERROR> Unable to get user info. The specified user does not exist. (Specified user does not exist)."; exit 1 ;;
"user add")
	IFS= read -r ak; IFS= read -r sk; printf '%s\n' "$sk" >> "$W/stdin"
	[ -n "${ADD_ERROR:-}" ] && { echo "mc: <ERROR> Unable to add new user. $ADD_ERROR"; exit 1; }
	printf '%s' "$sk" > "$W/user_$ak"; echo "Added user \` + "`" + `$ak\` + "`" + ` successfully."; exit 0 ;;
"user remove") rm -f "$W/user_$5"; exit 0 ;;
"policy attach")
	[ -n "${ATTACH_ERROR:-}" ] && { echo "mc: <ERROR> $ATTACH_ERROR"; exit 1; }
	echo "$5" > "$W/policy_$7"; exit 0 ;;
esac
echo "stub mc: unexpected: $*"; exit 2
`

const (
	initRootPass  = "Root-pass-0123456789-abcdefghij-XYZ"
	initAPISecret = "Api-secret-0123456789-abcdefghij-XYZ"
	initHandMade  = "Made-by-hand-secret-0123456789-abcdef"
)

type initRun struct {
	out, argv, stdin string
	code             int
	world            string
}

func (r initRun) has(name string) bool {
	_, err := os.Stat(filepath.Join(r.world, name))
	return err == nil
}

func (r initRun) read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.world, name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return strings.TrimSpace(string(b))
}

// runInit runs init.sh against stubMC, with the world prepared by seed and env on top of the
// variables the compose file passes.
func runInit(t *testing.T, seed map[string]string, env ...string) initRun {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found")
	}
	dir := t.TempDir()
	bin, world := filepath.Join(dir, "bin"), filepath.Join(dir, "world")
	for _, d := range []string{bin, world} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "mc"), []byte(stubMC), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"root_user": "root", "root_pass": initRootPass, "argv": "", "stdin": ""}
	for k, v := range seed {
		files[k] = v
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(world, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(bash, minioInitScript)
	cmd.Env = append([]string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + dir, "TMPDIR=" + dir, "WORLD=" + world,
		"MINIO_ROOT_USER=root", "MINIO_ROOT_PASSWORD=" + initRootPass,
		"API_ACCESS_KEY=apikey", "API_SECRET_KEY=" + initAPISecret,
		"UPLOAD_BUCKET=onecamp-uploads", "STORAGE_ATTACHED_BUCKET=",
		"MINIO_INIT_TRIES=1", "MINIO_INIT_PAUSE=0",
	}, env...)
	out, err := cmd.CombinedOutput()
	r := initRun{out: string(out), world: world}
	if ee, ok := err.(*exec.ExitError); ok {
		r.code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running %s: %v", minioInitScript, err)
	}
	r.argv, r.stdin = r.read(t, "argv"), r.read(t, "stdin")
	for _, secret := range []string{initRootPass, initAPISecret, initHandMade} {
		if strings.Contains(r.out, secret) {
			t.Errorf("init.sh printed a secret:\n%s", r.out)
		}
		if strings.Contains(r.argv, secret) {
			t.Errorf("init.sh put a secret on mc's command line, where any process list shows it:\n%s", r.argv)
		}
	}
	return r
}

func TestMinIOInitMakesWhatIsMissingAndNothingElse(t *testing.T) {
	t.Run("a fresh store gets its bucket and the API's key", func(t *testing.T) {
		r := runInit(t, nil)
		if r.code != 0 || !strings.Contains(r.out, "made access key apikey") {
			t.Fatalf("exit %d:\n%s", r.code, r.out)
		}
		if !r.has("bucket_onecamp-uploads") {
			t.Error("the bucket was not made")
		}
		if got := r.read(t, "user_apikey"); got != initAPISecret {
			t.Errorf("the key was made with the wrong secret (%d bytes)", len(got))
		}
		if got := r.read(t, "policy_apikey"); got != "readwrite" {
			t.Errorf("the key has policy %q, want readwrite", got)
		}
		if !strings.Contains(r.stdin, initAPISecret) {
			t.Error("the key's secret didn't reach mc on stdin")
		}
	})

	for _, tc := range []struct {
		name, want string
		seed       map[string]string
		env        []string
	}{
		{"a re-run leaves the key it made", "exists (a user)", map[string]string{"user_apikey": initAPISecret}, nil},
		{"a key made by hand as a user is left alone", "exists (a user)", map[string]string{"user_apikey": initHandMade}, nil},
		{"a key made by hand as a service account is left alone", "exists (a service account)", map[string]string{"svcacct_apikey": ""}, nil},
		{"an answer it can't read leaves the key alone", "couldn't tell", nil, []string{"USER_INFO_ERROR=connection reset by peer"}},
		{"the root key isn't made again", "root user", nil, []string{"API_ACCESS_KEY=root"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runInit(t, tc.seed, tc.env...)
			if r.code != 0 || !strings.Contains(r.out, tc.want) {
				t.Fatalf("exit %d, want 0 and %q:\n%s", r.code, tc.want, r.out)
			}
			if strings.Contains(r.argv, "admin user add") {
				t.Errorf("it called `mc admin user add`, which replaces an existing user's secret:\n%s", r.argv)
			}
			if secret, ok := tc.seed["user_apikey"]; ok {
				if got := r.read(t, "user_apikey"); got != secret {
					t.Error("an existing key's secret was changed")
				}
			}
			if !r.has("bucket_onecamp-uploads") {
				t.Error("the bucket was not made")
			}
		})
	}

	t.Run("files in an attached bucket aren't this machine's to set up", func(t *testing.T) {
		r := runInit(t, nil, "STORAGE_ATTACHED_BUCKET=ovh-bucket")
		if r.code != 0 || r.argv != "" {
			t.Fatalf("exit %d, mc called with:\n%s\n%s", r.code, r.argv, r.out)
		}
	})

	t.Run("a MinIO still restarting is waited for", func(t *testing.T) {
		r := runInit(t, map[string]string{"down_for": "2"}, "MINIO_INIT_TRIES=3")
		if r.code != 0 || !strings.Contains(r.out, "made access key apikey") {
			t.Fatalf("exit %d:\n%s", r.code, r.out)
		}
	})

	t.Run("a root it can't sign in as fails, so the API doesn't start against a broken store", func(t *testing.T) {
		r := runInit(t, map[string]string{"root_pass": "something-else-entirely"})
		if r.code == 0 || !strings.Contains(r.out, "FAILED") {
			t.Fatalf("exit %d, want non-zero:\n%s", r.code, r.out)
		}
	})

	t.Run("a key MinIO refuses is reported without its secret, and the API still starts", func(t *testing.T) {
		r := runInit(t, nil, "ADD_ERROR=secret "+initAPISecret+" is not allowed")
		if r.code != 0 || !strings.Contains(r.out, "NOT SET UP") || !strings.Contains(r.out, "<secret>") {
			t.Fatalf("exit %d:\n%s", r.code, r.out)
		}
	})

	t.Run("a key that can't get its policy is taken back, to be made again next start", func(t *testing.T) {
		r := runInit(t, nil, "ATTACH_ERROR=policy store unavailable")
		if r.code != 0 || !strings.Contains(r.out, "NOT SET UP") {
			t.Fatalf("exit %d:\n%s", r.code, r.out)
		}
		if r.has("user_apikey") {
			t.Error("the key without a policy was left in place, so no later start would ever fix it")
		}
	})
}
