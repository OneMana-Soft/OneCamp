package helpers

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every directory the shipped compose file mounts is made before the stack
// starts, with the owner its container writes as.
//
// WHAT WENT WRONG. Docker creates a missing mount source itself, as root:root
// 0755, and EMQX and OpenSearch run as uid 1000. So on every first install EMQX
// crash-looped on "mkdir: cannot create directory '/opt/emqx/data/configs':
// Permission denied", OpenSearch stopped on an AccessDeniedException, and the
// API exited because the broker never came up. build_restart_all ran
// `sudo chmod 777 -R ./data`, but BEFORE `up`, while those directories did not
// exist yet: only a second run mended an install, and it did so by making every
// datastore's files writable by every account on the machine.
//
// Now `make mount-dirs` (which build_restart_all runs first) makes each
// directory in MOUNT_DIRS and hands it to its owner. These tests keep that list
// in step with the compose file, and drive the real targets in a scratch
// directory with docker and sudo stood in for.

// mountLine is a bind mount from the install directory: `- ./src:/dst[:opts]`.
var (
	mountLine       = regexp.MustCompile(`^\s+- \./([^:\s]+):(/[^:\s]*)(:[a-z,]+)?\s*(#.*)?$`)
	mountServiceKey = regexp.MustCompile(`^  ([A-Za-z0-9_.-]+):\s*(#.*)?$`)
)

// composeMountDirs returns the directories distribute-compose.yml mounts from
// the install directory, each with the service that mounts it. A source whose
// last element has a dot in it (livekit.yaml, acl.conf) is a file,
// which the stack's own steps write, so it is not a directory to make.
func composeMountDirs(t *testing.T) map[string]string {
	t.Helper()
	f, err := os.Open("../distribute-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dirs := map[string]string{}
	service, inServices := "", false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "services:") {
			inServices = true
			continue
		}
		if inServices && line != "" && line[0] != ' ' && line[0] != '#' {
			inServices = false
		}
		if !inServices {
			continue
		}
		if m := mountServiceKey.FindStringSubmatch(line); m != nil {
			service = m[1]
			continue
		}
		m := mountLine.FindStringSubmatch(line)
		if m == nil || strings.Contains(path.Base(m[1]), ".") {
			continue
		}
		dirs[path.Clean(m[1])] = service
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	// The compose file mounts a dozen directories. Finding a handful means the
	// pattern stopped matching, and this guard would then pass on nothing.
	if len(dirs) < 10 {
		t.Fatalf("found %d mounted directories in distribute-compose.yml; the pattern no longer matches it: %v", len(dirs), dirs)
	}
	return dirs
}

// scratchInstall is a directory holding the customer Makefile, as an install does.
func scratchInstall(t *testing.T) string {
	t.Helper()
	mk, err := os.ReadFile("../Makefile-distribute")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), mk, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// makeVar returns a variable of the customer Makefile as make expands it, split into words.
func makeVar(t *testing.T, name string) []string {
	t.Helper()
	dir := scratchInstall(t)
	cmd := exec.Command("make", "--no-print-directory", "--eval",
		"print-make-var: ; @printf '%s\\n' $("+name+")", "print-make-var")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("reading %s: %v\n%s", name, err, out)
	}
	return strings.Fields(string(out))
}

// mountDirsList reads MOUNT_DIRS as make sees it: path -> owner.
func mountDirsList(t *testing.T) map[string]string {
	t.Helper()
	list := map[string]string{}
	for _, e := range makeVar(t, "MOUNT_DIRS") {
		p, owner, ok := strings.Cut(e, ":")
		if !ok {
			t.Fatalf("MOUNT_DIRS entry %q is not <path>:<owner>", e)
		}
		list[p] = owner
	}
	return list
}

// nonRootServices are the services whose images run as a fixed non-root user,
// so their directories need that owner: `docker run --rm --entrypoint id <image>`
// prints uid=1000(emqx) for emqx/emqx:6.0.0 and uid=1000(opensearch) for
// opensearchproject/opensearch:3.6.0. Every other service starts as root.
var nonRootServices = map[string]string{
	"emqx":             "1000:1000",
	"opensearch-node1": "1000:1000",
}

var ownerForm = regexp.MustCompile(`^(-|\d+:\d+)$`)

func TestEveryMountedDirectoryIsListedWithItsOwner(t *testing.T) {
	mounts, list := composeMountDirs(t), mountDirsList(t)
	for d, svc := range mounts {
		owner, ok := list[d]
		if !ok {
			t.Errorf("distribute-compose.yml's %s mounts ./%s, which MOUNT_DIRS in Makefile-distribute does not list. "+
				"Docker would make it as root:root on the first up; add it with the owner %s writes as", svc, d, svc)
			continue
		}
		if want, fixed := nonRootServices[svc]; fixed && owner != want {
			t.Errorf("MOUNT_DIRS gives ./%s the owner %q, but %s runs as %s and could not write there", d, owner, svc, want)
		}
	}
	for d, owner := range list {
		if !ownerForm.MatchString(owner) {
			t.Errorf("MOUNT_DIRS gives ./%s the owner %q; an owner is uid:gid, or - for whoever runs make", d, owner)
		}
		if _, ok := mounts[d]; !ok {
			t.Errorf("MOUNT_DIRS lists ./%s, which distribute-compose.yml no longer mounts", d)
		}
	}
}

// fakeMountDocker stands in for docker. It writes each call, one argument per
// line, to $DOCKER_CALLS and ends it with "--". It changes nothing, so a run
// whose chown it was asked for finds the owner still wrong afterwards, which is
// what a docker that "succeeded" without effect looks like. For `compose ... up`
// it also notes which of $EXPECT_DIRS were missing at that moment, and with
// $DOCKER_PULL_FAILS a `compose ... pull` fails as an image Docker Hub no longer
// has does.
const fakeMountDocker = `#!/bin/sh
case " $* " in
*" pull --help "*) echo "      --policy string          Apply pull policy"; exit 0 ;;
esac
printf '%s\n' "$@" -- >> "$DOCKER_CALLS"
case " $* " in
*" up "*)
	for d in $EXPECT_DIRS; do [ -d "$d" ] || echo "up: missing $d" >> "$DOCKER_CALLS"; done
	echo "up: checked" >> "$DOCKER_CALLS" ;;
*" pull "*)
	[ -n "$DOCKER_PULL_FAILS" ] && { echo "pull access denied for minio/minio" >&2; exit 1; } ;;
*" redis-cli ping "*) echo PONG ;;
*" /health/ready"*) echo "ok    database" ;;
*" redis-cli EVAL "*) echo 0 ;;
esac
exit 0
`

const fakeSudo = `#!/bin/sh
echo "sudo $*" >> "$SUDO_CALLS"
exit 1
`

// stubs puts fake docker and sudo first on PATH and returns the environment
// for make, plus where each wrote its calls.
func stubs(t *testing.T, extra ...string) (env []string, dockerCalls, sudoCalls string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fakeMountDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte(fakeSudo), 0o755); err != nil {
		t.Fatal(err)
	}
	logs := t.TempDir()
	dockerCalls, sudoCalls = filepath.Join(logs, "docker"), filepath.Join(logs, "sudo")
	env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"),
		"DOCKER_CALLS="+dockerCalls, "SUDO_CALLS="+sudoCalls)
	return append(env, extra...), dockerCalls, sudoCalls
}

func runMake(t *testing.T, dir string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("make", append([]string{"--no-print-directory"}, args...)...)
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// dockerRuns splits the fake docker's log into calls, each a list of arguments.
func dockerRuns(t *testing.T, file string) [][]string {
	t.Helper()
	b, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	var cur []string
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if l == "--" {
			calls = append(calls, cur)
			cur = nil
			continue
		}
		cur = append(cur, l)
	}
	return calls
}

func owner(uid, gid int) string { return fmt.Sprintf("%d:%d", uid, gid) }

// mount-dirs makes what is missing, asks Docker for exactly the owners that are
// wrong, and refuses to report success when an owner did not take.
func TestMountDirsMakesEachDirectoryAndChecksTheOwnerTook(t *testing.T) {
	uid, gid := os.Getuid(), os.Getgid()
	self, other := owner(uid, gid), owner(uid+1, gid+1)

	dir := scratchInstall(t)
	env, dockerCalls, sudoCalls := stubs(t)
	list := "MOUNT_DIRS=data/plain:- data/deep/mine:" + self + " data/theirs:" + other + " saml:-"
	out, err := runMake(t, dir, env, "mount-dirs", list)
	if err == nil {
		t.Fatalf("mount-dirs reported success with ./data/theirs still owned by %s, not %s:\n%s", self, other, out)
	}
	for _, d := range []string{"data/plain", "data/deep/mine", "data/theirs", "saml"} {
		if fi, err := os.Stat(filepath.Join(dir, d)); err != nil || !fi.IsDir() {
			t.Errorf("./%s was not made (%v)", d, err)
		}
	}
	runs := dockerRuns(t, dockerCalls)
	if len(runs) != 1 {
		t.Fatalf("docker ran %d times, want once, for the one wrong owner: %q", len(runs), runs)
	}
	want := []string{"run", "--rm", "--network", "none", "-v", filepath.Join(dir, "data") + ":/install/data",
		"-w", "/install", "alpine:3.22", "sh", "-c"}
	got := runs[0]
	if len(got) < len(want)+1 || strings.Join(got[:len(want)], " ") != strings.Join(want, " ") {
		t.Fatalf("docker was called as\n  docker %s\nwant it to start\n  docker %s ...", strings.Join(got, " "), strings.Join(want, " "))
	}
	if args := got[len(want)+1:]; strings.Join(args, " ") != "_ "+self+" data/theirs:"+other {
		t.Errorf("the container was asked to fix %q; want only the directory whose owner is wrong, after the operator's own uid:gid", args)
	}
	if !strings.Contains(out, "sudo chown -R "+other+" ./data/theirs") {
		t.Errorf("the failure doesn't name the command that mends it:\n%s", out)
	}
	if b, _ := os.ReadFile(sudoCalls); len(b) > 0 {
		t.Errorf("mount-dirs ran sudo: %s", b)
	}

	// Everything right: nothing to start, and it says so.
	_ = os.Remove(dockerCalls)
	out, err = runMake(t, dir, env, "mount-dirs", "MOUNT_DIRS=data/plain:- data/deep/mine:"+self)
	if err != nil {
		t.Fatalf("mount-dirs with every owner right: %v\n%s", err, out)
	}
	if runs := dockerRuns(t, dockerCalls); len(runs) != 0 {
		t.Errorf("mount-dirs started a container though every owner was right: %q", runs)
	}
	if !strings.Contains(out, "all present") {
		t.Errorf("mount-dirs with nothing to do said nothing:\n%s", out)
	}
}

// build_restart_all makes every mounted directory before `up`, and needs
// neither sudo nor a chmod 777 to do it.
func TestBuildRestartAllMakesTheMountsBeforeUp(t *testing.T) {
	mounts := composeMountDirs(t)
	// The real list, with each owner set to whoever runs the test, as if the
	// container's chown had taken: what is checked here is the order of things.
	var entries, expect []string
	for d, o := range mountDirsList(t) {
		if o != "-" {
			o = owner(os.Getuid(), os.Getgid())
		}
		entries = append(entries, d+":"+o)
	}
	for d := range mounts {
		expect = append(expect, d)
	}
	sort.Strings(entries)
	sort.Strings(expect)

	dir := scratchInstall(t)
	env, dockerCalls, sudoCalls := stubs(t, "EXPECT_DIRS="+strings.Join(expect, " "))
	out, err := runMake(t, dir, env, "build_restart_all", "MOUNT_DIRS="+strings.Join(entries, " "))
	if err != nil {
		t.Fatalf("make build_restart_all: %v\n%s", err, out)
	}
	log, _ := os.ReadFile(dockerCalls)
	if !strings.Contains(string(log), "up: checked") {
		t.Fatalf("build_restart_all never ran compose up:\n%s", log)
	}
	if strings.Contains(string(log), "up: missing") {
		t.Errorf("compose up ran before every mounted directory existed:\n%s", log)
	}
	if b, _ := os.ReadFile(sudoCalls); len(b) > 0 {
		t.Errorf("build_restart_all ran sudo, which prompts or fails on a server without passwordless sudo: %s", b)
	}
	if strings.Contains(out, "chmod 777") || strings.Contains(out, "chmod -R 777") {
		t.Errorf("build_restart_all still makes the data world-writable:\n%s", out)
	}
}

// composeVerb finds the first compose call whose subcommand is verb, by its
// place among the calls, or -1.
func composeVerb(calls [][]string, verb string) int {
	for i, c := range calls {
		if len(c) > 0 && c[0] == "compose" {
			for _, a := range c {
				if a == verb {
					return i
				}
			}
		}
	}
	return -1
}

// An update rebuilds every image built from source, then fetches every image the
// release names, and only then stops anything. A pull that fails leaves the
// running stack alone.
//
// WHAT WENT WRONG. build_restart_all ran `down` and then `up`, and up pulled
// what the new release named. minio/minio left Docker Hub, so up failed after
// down had stopped everything, and the workspace stayed down. And up -d builds a
// service's image only when there is none, so the shared-editing service ran the
// image of its install day through every update.
func TestBuildRestartAllFetchesBeforeItStopsAnything(t *testing.T) {
	var entries []string
	for d, o := range mountDirsList(t) {
		if o != "-" {
			o = owner(os.Getuid(), os.Getgid())
		}
		entries = append(entries, d+":"+o)
	}
	sort.Strings(entries)
	list := "MOUNT_DIRS=" + strings.Join(entries, " ")

	dir := scratchInstall(t)
	env, dockerCalls, _ := stubs(t)
	if out, err := runMake(t, dir, env, "build_restart_all", list); err != nil {
		t.Fatalf("make build_restart_all: %v\n%s", err, out)
	}
	calls := dockerRuns(t, dockerCalls)
	build, pull, down, up := composeVerb(calls, "build"), composeVerb(calls, "pull"), composeVerb(calls, "down"), composeVerb(calls, "up")
	if build < 0 || pull < 0 || down < 0 || up < 0 {
		t.Fatalf("want compose build, pull, down and up; docker was called as %q", calls)
	}
	if !(build < pull && pull < down && down < up) {
		t.Errorf("want build, then pull, then down, then up; got build #%d, pull #%d, down #%d, up #%d", build, pull, down, up)
	}
	if b := calls[build]; b[len(b)-1] != "--pull" {
		t.Errorf("compose build names services (%q); every service built from source must be rebuilt", b)
	}
	if p := strings.Join(calls[pull], " "); !strings.Contains(p, "--ignore-buildable") || !strings.Contains(p, "--policy missing") {
		t.Errorf("compose pull is %q; want --ignore-buildable --policy missing, so the API image built here is not looked for on Docker Hub", p)
	}

	// The pull fails every time: three tries, then nothing is stopped, and the
	// operator is told so.
	dir = scratchInstall(t)
	env, dockerCalls, _ = stubs(t, "DOCKER_PULL_FAILS=1")
	out, err := runMake(t, dir, env, "build_restart_all", list, "PULL_RETRY_PAUSE=0")
	if err == nil {
		t.Fatalf("build_restart_all carried on after a failed pull:\n%s", out)
	}
	calls = dockerRuns(t, dockerCalls)
	pulls := 0
	for _, c := range calls {
		if len(c) > 0 && c[0] == "compose" && strings.Contains(strings.Join(c, " "), " pull ") {
			pulls++
		}
	}
	if pulls != 3 {
		t.Errorf("a failing pull was tried %d times; want 3, so a download reset part way is resumed", pulls)
	}
	if composeVerb(calls, "down") >= 0 || composeVerb(calls, "up") >= 0 {
		t.Errorf("a failed pull still stopped or restarted the stack: %q", calls)
	}
	if !strings.Contains(out, "Nothing was stopped") {
		t.Errorf("a failed pull doesn't say the workspace is still running:\n%s", out)
	}
}

// compose.yml, which a bare `docker compose` in the install directory reads, is
// the definition just started. install copied it once, so after an update it
// still named the previous release's images (minio/minio, traefik:v3.0), and the
// documented `docker compose --profile workers up -d --scale go-worker=3` would
// have put them back. A copy that differed is kept as compose.yml.previous.
func TestBuildRestartAllKeepsComposeYmlInStep(t *testing.T) {
	var entries []string
	for d, o := range mountDirsList(t) {
		if o != "-" {
			o = owner(os.Getuid(), os.Getgid())
		}
		entries = append(entries, d+":"+o)
	}
	list := "MOUNT_DIRS=" + strings.Join(entries, " ")
	dir := scratchInstall(t)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("sample-compose.yml", "image: traefik:v3.7.13\n")
	write("compose.yml", "image: traefik:v3.0\n")
	env, _, _ := stubs(t)
	if out, err := runMake(t, dir, env, "build_restart_all", list); err != nil {
		t.Fatalf("make build_restart_all: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "compose.yml")); string(b) != "image: traefik:v3.7.13\n" {
		t.Errorf("compose.yml after an update is %q; want the release just started", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "compose.yml.previous")); string(b) != "image: traefik:v3.0\n" {
		t.Errorf("the old compose.yml was not kept as compose.yml.previous (got %q)", b)
	}

	// Already in step: nothing moves.
	_ = os.Remove(filepath.Join(dir, "compose.yml.previous"))
	if out, err := runMake(t, dir, env, "build_restart_all", list); err != nil {
		t.Fatalf("make build_restart_all again: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "compose.yml.previous")); err == nil {
		t.Error("a compose.yml already in step was moved aside")
	}
}

// An account that is not root, and that Docker refuses, is added to the docker
// group and the rest of the install runs under `sg docker`, with the domain and
// password it was given.
//
// WHAT WENT WRONG. get.docker.com lets only root and the docker group use
// Docker and adds nobody to that group, so `make install` run by the ubuntu
// user of a fresh Ubuntu 24.04 (or by OneCamp Cloud's provisioning account)
// wrote the configuration and stopped at its first Docker command: "permission
// denied while trying to connect to the docker API".
func TestInstallJoinsTheDockerGroupWhenDockerRefusesTheAccount(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root is never refused by Docker")
	}
	bin, logs := t.TempDir(), t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Docker refuses this account; getent says nobody is in the group yet; sudo
	// and sg note how they were called. sg runs nothing: what it was asked to
	// run is the evidence.
	write("docker", "#!/bin/sh\necho 'permission denied while trying to connect to the docker API at unix:///var/run/docker.sock' >&2\nexit 1\n")
	write("getent", "#!/bin/sh\necho 'docker:x:988:'\n")
	write("sudo", "#!/bin/sh\necho \"$*\" >> \""+logs+"/sudo\"\n")
	write("sg", "#!/bin/sh\nprintf '%s\\n' \"$@\" > \""+logs+"/sg\"\nenv | grep '^OC_' | sort > \""+logs+"/sg-env\"\n")

	dir := scratchInstall(t)
	if err := os.Remove(filepath.Join(dir, ".env")); err != nil {
		t.Fatal(err)
	}
	// A quote of each kind: the password used to be spliced into the recipe
	// between single quotes. ($ is make's own: a command-line value is expanded.)
	pw := `it's "a" pass!`
	env := append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	out, err := runMake(t, dir, env, "install", "EMAIL=admin@example.com", "DOMAIN=example.com", "PASSWORD="+pw)
	if err != nil {
		t.Fatalf("make install: %v\n%s", err, out)
	}
	user := os.Getenv("USER")
	if u, err := exec.Command("id", "-un").Output(); err == nil {
		user = strings.TrimSpace(string(u))
	}
	if b, _ := os.ReadFile(filepath.Join(logs, "sudo")); strings.TrimSpace(string(b)) != "usermod -aG docker "+user {
		t.Errorf("sudo was called as %q; want it to add %s to the docker group", b, user)
	}
	sg, _ := os.ReadFile(filepath.Join(logs, "sg"))
	args := strings.Split(strings.TrimRight(string(sg), "\n"), "\n")
	if len(args) != 3 || args[0] != "docker" || args[1] != "-c" || !strings.Contains(args[2], `install_run DOMAIN="$OC_DOMAIN"`) {
		t.Fatalf("the rest of the install did not run under `sg docker -c`: sg %q", args)
	}
	got, _ := os.ReadFile(filepath.Join(logs, "sg-env"))
	for _, want := range []string{"OC_DOMAIN=example.com", "OC_PW=" + pw} {
		if !strings.Contains(string(got), want+"\n") {
			t.Errorf("under sg, %q is missing from the environment:\n%s", want, got)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".env")); err == nil {
		t.Error("install_run also ran outside sg, where Docker refuses this account")
	}
}

// An install whose first `make install` never finished (a pull that failed, a
// crash loop) comes back by the documented update command once a fixed release
// is out. update said to start the stack, which brings the API up against a
// database that was never made; it now says to finish the install, with the
// email and domain already in .env.
func TestUpdateOfAnUnfinishedInstallSaysToFinishIt(t *testing.T) {
	bin := t.TempDir()
	// Docker answers, and nothing in this project has ever been created.
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := scratchInstall(t)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("ADMIN_EMAIL_ID=admin@example.com\nFE_DOMAIN=example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "version.txt"), []byte("v2.99.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	out, err := runMake(t, dir, env, "update")
	if err == nil {
		t.Fatalf("update carried on with nothing running:\n%s", out)
	}
	if !strings.Contains(out, "make install EMAIL=admin@example.com DOMAIN=example.com") {
		t.Errorf("update of an install that never ran doesn't say how to finish it:\n%s", out)
	}
	if strings.Contains(out, "make build_restart_all") {
		t.Errorf("update sent an unfinished install to build_restart_all:\n%s", out)
	}
	// An install that has run before keeps the old advice.
	if err := os.WriteFile(filepath.Join(dir, ".applied_version"), []byte("v2.98.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ = runMake(t, dir, env, "update")
	if !strings.Contains(out, "make build_restart_all") {
		t.Errorf("update of an install that has run doesn't say to start the stack:\n%s", out)
	}
}

// Redis keeps its data, every signed-in session among it, on the host.
//
// WHAT WENT WRONG. The compose file mounted ./data/redis_data/data, spelt with a
// Cyrillic "a", at /root/redis, which Redis never reads, and a redis.conf nothing wrote,
// which Docker made a directory. Redis's /data lived in the container, so every
// update and rebuild, which recreate it, signed everyone out.
func TestRedisKeepsItsDataOnTheHost(t *testing.T) {
	raw, err := os.ReadFile("../distribute-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	var redis []string
	in := false
	for _, line := range strings.Split(string(raw), "\n") {
		if m := mountServiceKey.FindStringSubmatch(line); m != nil {
			in = m[1] == "redis"
			continue
		}
		if in {
			redis = append(redis, line)
		}
	}
	block := strings.Join(redis, "\n")
	if !strings.Contains(block, "- ./data/redis_data/data:/data") {
		t.Error("redis does not mount ./data/redis_data/data at /data, where it keeps its data; every update would sign everyone out")
	}
	for _, line := range redis {
		if mountLine.MatchString(line) && strings.Contains(line, "redis.conf") {
			t.Error("redis mounts a redis.conf that nothing writes, so Docker makes it a directory")
		}
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if mountLine.MatchString(line) && strings.IndexFunc(line, func(r rune) bool { return r >= 0x0400 && r <= 0x04ff }) >= 0 {
			t.Errorf("a mount has a Cyrillic letter that reads as a Latin one, so its path is not the one it looks like: %s", strings.TrimSpace(line))
		}
	}
}

// An update keeps everyone signed in and starts every cache empty: between down
// and up, with only Redis started, build_restart_all deletes every Redis key but
// the sign-ins.
//
// WHY. Redis's data is on the host now (it was in the container, which every
// update replaced, so everyone was signed out and every cache began empty).
// Kept as it was, a cache an older release wrote, in its shape, would be read by
// the new one for up to its TTL.
func TestBuildRestartAllKeepsOnlySignInsInRedis(t *testing.T) {
	var entries []string
	for d, o := range mountDirsList(t) {
		if o != "-" {
			o = owner(os.Getuid(), os.Getgid())
		}
		entries = append(entries, d+":"+o)
	}
	dir := scratchInstall(t)
	env, dockerCalls, _ := stubs(t)
	if out, err := runMake(t, dir, env, "build_restart_all", "MOUNT_DIRS="+strings.Join(entries, " ")); err != nil {
		t.Fatalf("make build_restart_all: %v\n%s", err, out)
	}
	calls := dockerRuns(t, dockerCalls)
	down, eval, upAll := -1, -1, -1
	for i, c := range calls {
		args := strings.Join(c, " ")
		switch {
		case strings.HasSuffix(args, " down"):
			down = i
		case strings.Contains(args, " exec -T redis redis-cli EVAL "):
			eval = i
			if !strings.Contains(args, "string.find(k, '^auth:refresh')") || !strings.Contains(args, "redis.call('DEL', k)") {
				t.Errorf("the Redis clean-up does not keep exactly the sign-ins: %s", args)
			}
		case strings.HasSuffix(args, " up -d"):
			upAll = i
		}
	}
	if down < 0 || eval < 0 || upAll < 0 || !(down < eval && eval < upAll) {
		t.Errorf("want down, then Redis cleared but for the sign-ins, then up; got down #%d, clear #%d, up #%d in %q", down, eval, upAll, calls)
	}
}

// An older install's world-writable data is tightened, once, and an install
// without any starts nothing for it.
//
// WHAT WENT WRONG. build_restart_all ran `sudo chmod 777 -R ./data` on every
// rebuild until 14 Oct 2026, so every datastore stayed writable by every
// account on the machine; making directories with their owners fixed new
// installs, not old ones.
func TestMountDirsTakesOtherAccountsWriteOffOldData(t *testing.T) {
	self := owner(os.Getuid(), os.Getgid())
	list := "MOUNT_DIRS=data/plain:- data/mine:" + self
	dir := scratchInstall(t)
	if err := os.MkdirAll(filepath.Join(dir, "data", "plain"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "data", "plain"), 0o777); err != nil {
		t.Fatal(err)
	}
	env, dockerCalls, _ := stubs(t)
	out, err := runMake(t, dir, env, "mount-dirs", list)
	if err != nil {
		t.Fatalf("mount-dirs: %v\n%s", err, out)
	}
	want := "run --rm --network none -v " + filepath.Join(dir, "data") + ":/install/data alpine:3.22 chmod -R o-w /install/data"
	found := false
	for _, c := range dockerRuns(t, dockerCalls) {
		if strings.Join(c, " ") == want {
			found = true
		}
	}
	if !found {
		t.Errorf("world-writable ./data/plain was left as it was; want docker %s\n%s", want, out)
	}

	// Nothing world-writable: no container, nothing said about it.
	if err := os.Chmod(filepath.Join(dir, "data", "plain"), 0o775); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(dockerCalls)
	if out, err := runMake(t, dir, env, "mount-dirs", list); err != nil {
		t.Fatalf("mount-dirs: %v\n%s", err, out)
	}
	for _, c := range dockerRuns(t, dockerCalls) {
		if strings.Contains(strings.Join(c, " "), "chmod -R o-w") {
			t.Errorf("mount-dirs tightened ./data with nothing world-writable in it (group write is the operator's umask)")
		}
	}
}
