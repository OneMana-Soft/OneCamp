package helpers

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// A release's files all carry the archive's one date, so every build from the
// install directory gives them real times first (Makefile-distribute's
// archive-dates).
//
// WHY. BuildKit knows a file in a build's context by its size and date, and the
// archive dates every file 1980. A file that changed between releases but kept
// its size looked unchanged, so the build used the copy it had cached from the
// release before: v2.73.1's update built the collaboration service from
// v2.73.0's package.json (axios ^1.13.2 and ^1.20.0 are the same length) beside
// v2.73.1's lockfile, and pnpm stopped the update (the release proof, 17 Oct
// 2026). A stale image needn't fail at all: it can run the old code, quietly.

// localBuilds are the recipes that build an image from files in the install
// directory. web_up isn't one: the web app builds from the public repository.
var localBuilds = []string{"build_restart_all", "build_restart_service", "coding-up", "stt_up"}

var localBuildLine = regexp.MustCompile(`(?m)^\t[^\n]*(docker build |\$\(COMPOSE\)[^\n]* build\b|up -d --build)`)

func TestEveryLocalBuildGivesTheArchiveFilesRealDatesFirst(t *testing.T) {
	raw, err := os.ReadFile("../Makefile-distribute")
	if err != nil {
		t.Fatal(err)
	}
	mk := string(raw)
	dates := recipeOf(t, mk, "archive-dates")
	for _, want := range []string{"! -newermt '1981-01-01'", "-path ./data -prune", "touch -c"} {
		if !strings.Contains(dates, want) {
			t.Errorf("archive-dates lacks %q, so it no longer touches just the archive's files", want)
		}
	}
	for _, target := range localBuilds {
		r := recipeOf(t, mk, target)
		build := localBuildLine.FindStringIndex(r)
		if build == nil {
			t.Errorf("%s builds nothing from the install directory any more; take it off localBuilds", target)
			continue
		}
		if at := strings.Index(r, "archive-dates"); at < 0 || at > build[0] {
			t.Errorf("%s builds before archive-dates, so a changed file that kept its size builds from the last release's copy", target)
		}
	}
	// No other recipe builds from the install directory without it. A rule runs
	// from its target's line to the next target's line.
	starts := regexp.MustCompile(`(?m)^([A-Za-z0-9_-]+):`).FindAllStringSubmatchIndex(mk, -1)
	for i, s := range starts {
		end := len(mk)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		name := mk[s[2]:s[3]]
		if !localBuildLine.MatchString(mk[s[0]:end]) {
			continue
		}
		known := false
		for _, target := range localBuilds {
			known = known || name == target
		}
		if !known {
			t.Errorf("%s builds an image from the install directory and isn't on localBuilds", name)
		}
	}
}

// What archive-dates does, run: the archive's files get the time of now, and the
// install's own files (data/ above all) keep theirs.
func TestArchiveDatesTouchesOnlyTheArchivesFiles(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("no make here")
	}
	mk, err := filepath.Abs("../Makefile-distribute")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	archive := time.Date(1980, 1, 1, 0, 0, 0, 0, time.Local)
	written := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	files := map[string]time.Time{
		"other-services/collaboration-service/package.json": archive,
		"app":                           archive,
		"data/postgres_data/PG_VERSION": archive,
		".env":                          written,
		"compose.yml":                   written,
	}
	for name, when := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("X=1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("make", "-s", "-f", mk, "archive-dates")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make archive-dates: %v\n%s", err, out)
	}
	since := time.Now().Add(-time.Hour)
	for name, when := range files {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		fromArchive := when.Equal(archive) && !strings.HasPrefix(name, "data/")
		switch {
		case fromArchive && info.ModTime().Before(since):
			t.Errorf("%s still has the archive's date, so a build would still trust its cached copy", name)
		case !fromArchive && !info.ModTime().Equal(when):
			t.Errorf("%s was touched (%v), but it's the install's own file", name, info.ModTime())
		}
	}
}
