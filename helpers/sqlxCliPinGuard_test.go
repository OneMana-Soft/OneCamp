package helpers

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every line that installs sqlx-cli names one exact version, builds it from its published lock file, and runs in
// one exact Rust image.
//
// WHY. `make update` applies the schema with `cargo install sqlx-cli ... && sqlx migrate run` in a throwaway
// container, so every customer's update compiled whatever sqlx-cli crates.io served that day: 0.9.0 on 25 Oct
// 2026, while the 0.8.0 that wrote many installs' migration ledgers was still about. A sqlx-cli release that
// changed how migrations are checked or recorded would stop every install's next update at step 2/4 (migrate),
// and nothing we release could call it back. Pinned, an update runs the sqlx-cli the release was proven with:
// 0.9.0 applies every migration its ledger lacks, in order, whatever the highest applied one is, and accepts a
// ledger 0.8.0 wrote (release-1011/logs/release-2756/migq.log and migx.log). Moving it becomes a change someone
// makes and proves.
//
// --locked builds the dependencies the published lock file names, so a dependency's new release can't change the
// tool either. That leaves the compiler as the one thing still moving: a later Rust that can't build one of those
// locked dependencies would stop the update just the same, so the image is an exact version tag too, never
// rust:alpine. One version and one image on every line: the demo's migrate_server_up and migrate_server_down run
// the same install as the customers' migrate_up, and the old onboarding notes, kept as history, show the same
// command for anyone who copies it.
func TestSqlxCliInstallsArePinned(t *testing.T) {
	install := regexp.MustCompile(`cargo install sqlx-cli\b[^"&;]*`)
	pinned := regexp.MustCompile(`^cargo install sqlx-cli --version =(\d+\.\d+\.\d+) --locked `)
	image := regexp.MustCompile(`\brust:([A-Za-z0-9._-]+)`)
	exactImage := regexp.MustCompile(`^\d+\.\d+\.\d+-alpine\d+\.\d+$`)
	versions := map[string][]string{}
	images := map[string][]string{}
	// The recipes that run, with the install lines each must hold (migrate_server_up and migrate_server_down;
	// the customers' migrate_up), and the notes kept as history, which may be retired. Fewer lines in a recipe
	// means an install moved out of this check's sight (into a variable, another file), not that it went away.
	files := []struct {
		path string
		min  int
	}{{"../Makefile", 2}, {"../Makefile-distribute", 1}, {"../docs/customer-onboarding.md", 0}}
	for _, f := range files {
		raw, err := os.ReadFile(f.path)
		if err != nil {
			if f.min == 0 && os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
		found := 0
		for i, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			at := fmt.Sprintf("%s:%d", strings.TrimPrefix(f.path, "../"), i+1)
			for _, m := range install.FindAllString(line, -1) {
				found++
				p := pinned.FindStringSubmatch(m)
				if p == nil {
					t.Errorf("%s installs sqlx-cli without a pin: %q; name it as `cargo install sqlx-cli --version =X.Y.Z --locked ...`", at, strings.TrimSpace(m))
					continue
				}
				versions[p[1]] = append(versions[p[1]], at)
			}
			for _, m := range image.FindAllStringSubmatch(line, -1) {
				if !exactImage.MatchString(m[1]) {
					t.Errorf("%s runs rust:%s; name an exact image, rust:X.Y.Z-alpineA.B", at, m[1])
					continue
				}
				images[m[1]] = append(images[m[1]], at)
			}
		}
		if found < f.min {
			t.Errorf("found %d sqlx-cli install lines in %s, want at least %d", found, strings.TrimPrefix(f.path, "../"), f.min)
		}
	}
	if len(versions) > 1 {
		t.Errorf("the install lines pin different sqlx-cli versions: %v", versions)
	}
	if len(images) > 1 {
		t.Errorf("the install lines run different rust images: %v", images)
	}
	if len(versions) == 1 && len(images) == 0 {
		t.Errorf("no exact rust image runs the pinned sqlx-cli")
	}
}
