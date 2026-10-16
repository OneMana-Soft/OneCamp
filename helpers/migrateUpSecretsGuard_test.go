package helpers

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// make migrate_up gives sqlx the migrations and nothing else, and the database
// password on no command line.
//
// WHAT WENT WRONG. The sqlx container was given the whole install directory,
// .env included. sqlx loads a .env it finds, can't parse LIVEKIT_API_KEY's
// "name: secret" value, and its warning quotes the value: every install and
// every update printed the LiveKit API secret on the terminal (a test server,
// 16 Oct 2026). The database URL was an argument, so the password was in the
// process list and in the container's recorded command.
//
// The recipe runs here against a stand-in docker that answers the two lookups
// migrate_up makes and records what the sqlx container would have been given.
func TestMigrateUpGivesSqlxOnlyTheMigrations(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed")
	}
	dir := scratchInstall(t)
	const (
		dbPassword  = "db-pw-0f3a9c"
		livekitPass = "lk-secret-7d21e4"
	)
	env := "DB_USER=onecamp\nDB_PASSWORD=" + dbPassword + "\nDB_NAME=onecamp\n" +
		"LIVEKIT_API_KEY=onecamp: " + livekitPass + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	runLog := filepath.Join(bin, "run.log")
	fake := `#!/bin/sh
case "$1" in
  compose) case " $* " in *" ps "*) echo cid0123;; esac; exit 0;;
  inspect) echo "onecamp_onecamp-shared-net "; exit 0;;
  run) for a in "$@"; do printf 'ARG %s\n' "$a"; done > "` + runLog + `"
       printf 'ENV DATABASE_URL=%s\n' "$DATABASE_URL" >> "` + runLog + `"; exit 0;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("make", "--no-print-directory", "migrate_up")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make migrate_up: %v\n%s", err, out)
	}
	for _, secret := range []string{dbPassword, livekitPass} {
		if strings.Contains(string(out), secret) {
			t.Errorf("make migrate_up printed a secret:\n%s", out)
		}
	}
	raw, err := os.ReadFile(runLog)
	if err != nil {
		t.Fatalf("the sqlx container was never started: %v\n%s", err, out)
	}
	var args []string
	var gotURL string
	for _, l := range strings.Split(string(raw), "\n") {
		if a, ok := strings.CutPrefix(l, "ARG "); ok {
			args = append(args, a)
		}
		if v, ok := strings.CutPrefix(l, "ENV DATABASE_URL="); ok {
			gotURL = v
		}
	}
	argv := strings.Join(args, " ")
	if strings.Contains(argv, dbPassword) {
		t.Errorf("the database password is on the sqlx container's command line: %s", argv)
	}
	for i, a := range args {
		if a != "-v" || i+1 == len(args) {
			continue
		}
		src, _, _ := strings.Cut(args[i+1], ":")
		if filepath.Clean(src) != filepath.Join(dir, "migrations") {
			t.Errorf("the sqlx container mounts %s; only %s may go in (the install directory holds .env)", src, filepath.Join(dir, "migrations"))
		}
	}
	if !strings.Contains(argv, filepath.Join(dir, "migrations")+":/app/migrations:ro") {
		t.Errorf("the migrations are not mounted read-only at /app/migrations: %s", argv)
	}
	if !strings.Contains(" "+argv+" ", " -e DATABASE_URL ") {
		t.Errorf("DATABASE_URL is not passed to the container by name: %s", argv)
	}
	if want := "postgres://onecamp:" + dbPassword + "@postgres:5432/onecamp?sslmode=disable"; gotURL != want {
		t.Errorf("DATABASE_URL in docker's environment = %q, want the install's database", gotURL)
	}
}

// The dev Makefile's migration targets, which the demo's deploys run, follow
// the same rule: migrate_server_up and migrate_server_down mounted the whole
// checkout and gave the URL as an argument, which make also echoed, and
// migrate_up and migrate_down ran sqlx in the checkout, beside its .env, with
// the URL on the command line.
func TestDevMigrationTargetsKeepThePasswordAndDotenvAway(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed")
	}
	mk, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	const dbPassword = "dev-db-pw-58c1"
	files := map[string]string{
		"Makefile": string(mk),
		".env":     "DB_USER=onecamp\nDB_PASSWORD=" + dbPassword + "\nDB_NAME=onecamp\nDB_HOST=localhost\nDB_PORT=5432\nLIVEKIT_API_KEY=onecamp: lk-dev-secret-0d9e\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "calls.log")
	// docker and sqlx record their arguments and the URL they were given by
	// name; sqlx also whether a .env sits where it runs, which it would load.
	// (The docker CLI runs in the checkout; its container gets migrations/.)
	for _, tool := range []string{"docker", "sqlx"} {
		dotenv := "true"
		if tool == "sqlx" {
			dotenv = `[ -e .env ] && printf 'DOTENV in %s\n' "$(pwd)"`
		}
		body := `#!/bin/sh
{ printf '` + tool + `'; for a in "$@"; do printf ' [%s]' "$a"; done; printf '\n'
  printf 'ENV DATABASE_URL=%s\n' "$DATABASE_URL"
  ` + dotenv + `; } >> "` + log + `"
exit 0
`
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env := append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, target := range []string{"migrate_server_up", "migrate_server_down", "migrate_up", "migrate_down"} {
		if err := os.Remove(log); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		cmd := exec.Command("make", "--no-print-directory", target, "ONECAMP_NET=onecamp-beta_onecamp-shared-net")
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("make %s: %v\n%s", target, err, out)
		}
		if strings.Contains(string(out), dbPassword) {
			t.Errorf("make %s printed the database password:\n%s", target, out)
		}
		raw, err := os.ReadFile(log)
		if err != nil {
			t.Fatalf("make %s ran neither docker nor sqlx:\n%s", target, out)
		}
		calls := string(raw)
		for _, l := range strings.Split(calls, "\n") {
			if (strings.HasPrefix(l, "docker ") || strings.HasPrefix(l, "sqlx ")) && strings.Contains(l, dbPassword) {
				t.Errorf("make %s put the database password on a command line: %s", target, l)
			}
		}
		if strings.Contains(calls, "DOTENV in ") {
			t.Errorf("make %s ran sqlx beside a .env, which it loads:\n%s", target, calls)
		}
		if !strings.Contains(calls, "ENV DATABASE_URL=postgres://onecamp:"+dbPassword+"@") {
			t.Errorf("make %s did not hand sqlx the database URL by name:\n%s", target, calls)
		}
		if strings.HasPrefix(target, "migrate_server") {
			if !strings.Contains(calls, "["+filepath.Join(dir, "migrations")+":/app/migrations:ro]") || strings.Contains(calls, "["+dir+":/app]") {
				t.Errorf("make %s does not give the container migrations/ alone, read-only:\n%s", target, calls)
			}
		}
	}
}
