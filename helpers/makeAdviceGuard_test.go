package helpers

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every "make <target>" the customer Makefile tells an operator to run is a
// target it has.
//
// verify told someone whose web app answered with an error to "check make
// logs", and there was no logs target: the advice ended in "No rule to make
// target". A target named in advice is checked here against the Makefile's own
// rules, and logs is run to show it reads the service it is given.
func TestTheMakefileOnlyAdvisesTargetsItHas(t *testing.T) {
	mk, err := os.ReadFile("../Makefile-distribute")
	if err != nil {
		t.Fatal(err)
	}
	src := string(mk)
	rules := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^([a-zA-Z0-9_-]+):`).FindAllStringSubmatch(src, -1) {
		rules[m[1]] = true
	}
	// Advice is in what echo prints: "run make X", "check make X", "then: make X".
	advice := regexp.MustCompile(`(?:run|check|then:?|Run:?)\s+(?:sudo\s+)?make\s+([a-z][a-z0-9_-]+)`)
	for _, line := range strings.Split(src, "\n") {
		if !strings.Contains(line, "echo") {
			continue
		}
		for _, m := range advice.FindAllStringSubmatch(line, -1) {
			if !rules[m[1]] {
				t.Errorf("the Makefile advises make %s, which it has no rule for:\n%s", m[1], strings.TrimSpace(line))
			}
		}
	}

	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), mk, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("make", "--no-print-directory", "-n", "logs", "SERVICE=web", "LINES=7")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "logs --tail 7 web") {
		t.Fatalf("make logs SERVICE=web LINES=7 should show web's last 7 lines (%v):\n%s", err, out)
	}
}
