package onecamp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scripts/migration-parity.sh is the release's check that the two editions
// ship the same migrations, file for file and byte for byte: a workspace can
// move between them, and sqlx refuses a database holding a migration the build
// lacks or one whose bytes changed. The lines drifted on 9 Oct 2026 (four
// missing) and on 15 Oct (214). This runs the script on a throwaway repository
// with two lines, so the check itself is proven to fail where it must.
func TestMigrationParityRefusesEditionsThatDiffer(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("git is needed to test the migration parity check")
	}
	script, err := filepath.Abs("scripts/migration-parity.sh")
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = repo
		// The developer's own git settings (hooks, signing) stay out of it.
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(repo, "migrations", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Each line is committed on a detached HEAD and then named, so naming one
	// never moves another.
	commit := func(line string) {
		t.Helper()
		git("add", "-A")
		git("commit", "-q", "-m", line)
		git("update-ref", "refs/heads/"+line, "HEAD")
	}
	from := func(line string) { t.Helper(); git("checkout", "-q", "--detach", line) }

	git("init", "-q", "-b", "start")
	write("001_start.up.sql", "CREATE TABLE a (id int);\n")
	write("001_start.down.sql", "DROP TABLE a;\n")
	write("002_more.up.sql", "ALTER TABLE a ADD COLUMN b int;\n")
	write("002_more.down.sql", "ALTER TABLE a DROP COLUMN b;\n")
	commit("same")
	// twin: the same migrations under different code, as the two editions are.
	from("same")
	if err := os.WriteFile(filepath.Join(repo, "edition.go"), []byte("package edition\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit("twin")
	from("same")
	write("003_new.up.sql", "CREATE TABLE c (id int);\n")
	write("003_new.down.sql", "DROP TABLE c;\n")
	commit("ahead")
	from("same")
	write("002_more.up.sql", "ALTER TABLE a ADD COLUMN b bigint;\n")
	commit("edited")
	git("checkout", "-q", "--orphan", "empty")
	git("rm", "-q", "-r", "--cached", ".")
	if err := os.RemoveAll(filepath.Join(repo, "migrations")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("no migrations here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit("empty")

	parity := func(args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command("bash", append([]string{script}, args...)...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if exit, ok := err.(*exec.ExitError); ok {
			return string(out), exit.ExitCode()
		} else if err != nil {
			t.Fatalf("running the script: %v", err)
		}
		return string(out), 0
	}

	for _, c := range []struct {
		name string
		args []string
		code int
		says []string
	}{
		{"the same files pass", []string{"same", "twin"}, 0, []string{"hold the same 4 migration files", "versions 1 to 2"}},
		{"a migration only one line has fails", []string{"ahead", "same"}, 1, []string{"only in ahead", "migrations/003_new.up.sql", "migrations/003_new.down.sql", "FAIL"}},
		{"it fails whichever line is missing it", []string{"same", "ahead"}, 1, []string{"only in ahead", "migrations/003_new.up.sql"}},
		{"the same name with other bytes fails", []string{"same", "edited"}, 1, []string{"different bytes", "migrations/002_more.up.sql", "FAIL"}},
		{"a line pushed first may be ahead", []string{"--allow-ahead", "ahead", "same"}, 0, []string{"ahead in ahead", "allowed: versions 3", "holds all 4 migration files of same"}},
		{"but never behind", []string{"--allow-ahead", "same", "ahead"}, 1, []string{"only in ahead", "migrations/003_new.up.sql"}},
		{"nor with other bytes", []string{"--allow-ahead", "ahead", "edited"}, 1, []string{"different bytes", "migrations/002_more.up.sql"}},
		{"a line with no migrations compares nothing, so it fails", []string{"empty", "same"}, 2, []string{"holds no migrations"}},
		{"an unknown ref fails", []string{"same", "no-such-line"}, 2, []string{"no commit named no-such-line"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, code := parity(c.args...)
			if code != c.code {
				t.Errorf("exit %d; want %d\n%s", code, c.code, out)
			}
			for _, s := range c.says {
				if !strings.Contains(out, s) {
					t.Errorf("the output doesn't say %q:\n%s", s, out)
				}
			}
		})
	}
}
