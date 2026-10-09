package helpers

import (
	"crypto/tls"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// SAML on a self-hosted install: `make saml-cert` makes the key pair in ./saml
// and points the API at it as /app/saml, which the shipped compose file mounts.
// Before, the key could not reach the container at all (.dockerignore leaves
// *.key out of the image), so SAML, which the licence sells, needed the compose
// file edited by hand. Driven, not read: the real target runs in a temp dir.
func TestSAMLCertIsMadeKeptAndMountedWhereTheAPIReadsIt(t *testing.T) {
	for _, bin := range []string{"make", "openssl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not installed", bin)
		}
	}
	mk, err := os.ReadFile("../Makefile-distribute")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), mk, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("BACKEND_DOMAIN=api.example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func() string {
		t.Helper()
		cmd := exec.Command("make", "--no-print-directory", "saml-cert")
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("make saml-cert: %v\n%s", err, out)
		}
		return string(out)
	}
	// What to run next is what reads a changed .env in seconds (restart-api),
	// as the help says, not a rebuild that compiles the migration tool first.
	if out := run(); !strings.Contains(out, "make restart-api") || strings.Contains(out, "build_restart_service") {
		t.Errorf("make saml-cert doesn't send the operator to make restart-api:\n%s", out)
	}
	crt, key := filepath.Join(dir, "saml", "sp.crt"), filepath.Join(dir, "saml", "sp.key")
	if _, err := tls.LoadX509KeyPair(crt, key); err != nil {
		t.Fatalf("the pair the API loads (tls.LoadX509KeyPair, as services/SAML does): %v", err)
	}
	env, _ := os.ReadFile(filepath.Join(dir, ".env"))
	for _, want := range []string{"SAML_SP_CERT_PATH=/app/saml/sp.crt", "SAML_SP_KEY_PATH=/app/saml/sp.key"} {
		if !strings.Contains(string(env), want) {
			t.Errorf(".env lacks %s:\n%s", want, env)
		}
	}
	before, _ := os.ReadFile(crt)
	run()
	if after, _ := os.ReadFile(crt); string(after) != string(before) {
		t.Fatal("a second run replaced the certificate the identity provider holds a copy of")
	}

	compose, err := os.ReadFile("../distribute-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^\s+- \./saml:/app/saml:ro$`).Match(compose) {
		t.Error("the shipped compose file does not mount ./saml at /app/saml, where the paths point")
	}

	// OneCamp's SAML requests go out unsigned (services/SAML sets no
	// SignRequest); the certificate is in its metadata for the provider to
	// encrypt to. The help said the pair was what sign-in "signs with".
	help := exec.Command("make", "--no-print-directory", "help")
	help.Dir = dir
	out, err := help.CombinedOutput()
	if err != nil {
		t.Fatalf("make help: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "saml-cert") && regexp.MustCompile(`\bsign(s|ed|ing)\b`).MatchString(line) {
			t.Errorf("make help says SAML signs with the pair: %q", line)
		}
	}
}

// fakeDocker stands in for docker on PATH: it notes how it was called, then
// does to the mounted directory what the container does. Root in there can
// hand the directory to the operator; the stand-in isn't root, so it gives
// the directory's owner, the test, write access back, which is the effect the
// target relies on. DOCKER_FAILS makes it fail, as docker does for a user who
// may not use it.
const fakeDocker = `#!/bin/sh
printf '%s\n' "$@" >> "$DOCKER_CALLS"
[ -n "$DOCKER_FAILS" ] && exit 1
while [ $# -gt 0 ]; do
	[ "$1" = "-v" ] && { src=${2%%:*}; shift; }
	shift
done
chmod u+w "$src"
`

// After the stack's first start, ./saml is root's: Docker creates a mount's
// missing source itself, as root. An operator who isn't root then got "openssl
// could not make the key pair" from make saml-cert. The target now has Docker
// hand the directory over and makes the pair; the directory here is the
// operator's own but not writable, which is what the target can see of one
// root owns.
func TestSAMLCertIsMadeInTheDirectoryDockerMade(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write in a directory root owns, so the case can't arise")
	}
	for _, bin := range []string{"make", "openssl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not installed", bin)
		}
	}
	mk, err := os.ReadFile("../Makefile-distribute")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	// install is an install directory whose ./saml Docker made: there, empty,
	// and not writable by whoever runs make.
	install := func() string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "Makefile"), mk, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("BACKEND_DOMAIN=api.example.com\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		saml := filepath.Join(dir, "saml")
		if err := os.Mkdir(saml, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(saml, 0o755) }) // so the temp dir can be removed
		return dir
	}
	run := func(dir string, env ...string) (string, error) {
		cmd := exec.Command("make", "--no-print-directory", "saml-cert")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), append([]string{"PATH=" + bin + ":" + os.Getenv("PATH")}, env...)...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	dir := install()
	calls := filepath.Join(t.TempDir(), "calls")
	if out, err := run(dir, "DOCKER_CALLS="+calls); err != nil {
		t.Fatalf("make saml-cert in a ./saml Docker made: %v\n%s", err, out)
	}
	crt, key := filepath.Join(dir, "saml", "sp.crt"), filepath.Join(dir, "saml", "sp.key")
	if _, err := tls.LoadX509KeyPair(crt, key); err != nil {
		t.Fatalf("the pair the API loads: %v", err)
	}
	if fi, err := os.Stat(key); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the key's mode is %v (%v), want 0600", fi.Mode().Perm(), err)
	}
	called, _ := os.ReadFile(calls)
	args := strings.Split(strings.TrimSpace(string(called)), "\n")
	owner := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	want := []string{"run", "--rm", "--network", "none", "-v", filepath.Join(dir, "saml") + ":/saml", "alpine:3.22", "chown", "-R", owner, "/saml"}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Fatalf("docker was called as\n  docker %s\nwant\n  docker %s", strings.Join(args, " "), strings.Join(want, " "))
	}
	// Kept, as anywhere else, and without Docker this time: the directory is
	// the operator's now.
	before, _ := os.ReadFile(crt)
	if out, err := run(dir, "DOCKER_CALLS="+calls); err != nil {
		t.Fatalf("a second make saml-cert: %v\n%s", err, out)
	}
	if after, _ := os.ReadFile(crt); string(after) != string(before) {
		t.Fatal("a second run replaced the certificate the identity provider holds a copy of")
	}
	if again, _ := os.ReadFile(calls); string(again) != string(called) {
		t.Errorf("a second run, with the pair there, called docker again:\n%s", again)
	}

	// Docker refused (this user may not use it): say what to run, make nothing.
	dir = install()
	out, err := run(dir, "DOCKER_CALLS="+filepath.Join(t.TempDir(), "calls"), "DOCKER_FAILS=1")
	if err == nil || !strings.Contains(out, "sudo make saml-cert") {
		t.Fatalf("with Docker refusing, make saml-cert: %v\n%s\nwant a failure that says to run sudo make saml-cert", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "saml", "sp.key")); err == nil {
		t.Error("a key was made though the directory was never handed over")
	}
}
