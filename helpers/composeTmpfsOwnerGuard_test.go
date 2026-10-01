package helpers

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A tmpfs a non-root service must write to has to say who owns it.
//
// WHY THIS IS A TEST. A tmpfs mount replaces the directory the image prepared, and
// comes up owned by root with mode 755 unless its options say otherwise. The
// runners' Dockerfiles carefully `chown` /work to their unprivileged user, and the
// compose files then mounted a tmpfs over it, so the chown never took effect. Every
// coding run failed with "mkdir /work/coderun-…: permission denied", surfaced to
// people as "The coding runner failed (500)", from July until 29 Sep 2026. Nothing
// else noticed: the container was healthy and listening the whole time.
//
// /tmp is the one exception: Docker keeps the image's 1777 on it, which any user can
// write. Everything else needs uid= (or an explicit mode=) in its options.

// The coding runner's compose file exists only where the coding runner is
// developed; the editions that ship without it still carry the plain runner in
// the deployed files, so it is checked when present rather than required.
const optionalCodingCompose = "../code-runner-coding-compose.yml"

func tmpfsOwnerComposeFiles() []string {
	files := append([]string{}, deployedComposeFiles...)
	if _, err := os.Stat(optionalCodingCompose); err == nil {
		files = append(files, optionalCodingCompose)
	}
	return files
}

var (
	composeUserLine  = regexp.MustCompile(`^\s+user:\s*"?(\d+)(?::\d+)?"?\s*$`)
	composeTmpfsKey  = regexp.MustCompile(`^(\s+)tmpfs:\s*$`)
	composeListEntry = regexp.MustCompile(`^\s+-\s*"?([^"\s]+)"?\s*$`)
)

type tmpfsService struct {
	user   string
	mounts []string
}

func tmpfsServices(t *testing.T, path string) map[string]*tmpfsService {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	out := map[string]*tmpfsService{}
	inServices, current, tmpfsIndent := false, "", -1
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "services:") {
			inServices = true
			continue
		}
		if inServices && composeTopLevelKey.MatchString(line) {
			inServices = false
		}
		if !inServices {
			continue
		}
		if m := composeServiceLine.FindStringSubmatch(line); m != nil {
			current, tmpfsIndent = m[1], -1
			out[current] = &tmpfsService{}
			continue
		}
		if current == "" {
			continue
		}
		if m := composeUserLine.FindStringSubmatch(line); m != nil {
			out[current].user = m[1]
		}
		if m := composeTmpfsKey.FindStringSubmatch(line); m != nil {
			tmpfsIndent = len(m[1])
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if tmpfsIndent >= 0 {
			indent := len(line) - len(strings.TrimLeft(line, " "))
			if m := composeListEntry.FindStringSubmatch(line); m != nil && indent >= tmpfsIndent {
				out[current].mounts = append(out[current].mounts, m[1])
				continue
			}
			if strings.TrimSpace(line) != "" {
				tmpfsIndent = -1
			}
		}
	}
	return out
}

func TestNonRootTmpfsMountsSayWhoOwnsThem(t *testing.T) {
	sawRunner := false
	for _, path := range tmpfsOwnerComposeFiles() {
		t.Run(strings.TrimPrefix(path, "../"), func(t *testing.T) {
			var offenders []string
			for name, svc := range tmpfsServices(t, path) {
				if strings.Contains(name, "code-runner") && len(svc.mounts) > 0 {
					sawRunner = true
				}
				if svc.user == "" || svc.user == "0" {
					continue
				}
				for _, m := range svc.mounts {
					target, opts, _ := strings.Cut(m, ":")
					if target == "/tmp" {
						continue
					}
					if !strings.Contains(opts, "uid="+svc.user) && !strings.Contains(opts, "mode=") {
						offenders = append(offenders, fmt.Sprintf("%s: %s (runs as %s)", name, target, svc.user))
					}
				}
			}
			sort.Strings(offenders)
			if len(offenders) > 0 {
				t.Errorf("tmpfs mounts come up owned by root, so these users cannot write to them; add "+
					"uid=<user>,gid=<group> (and a mode) to the options:\n  %s", strings.Join(offenders, "\n  "))
			}
		})
	}
	if !sawRunner {
		t.Fatal("the parser found no code runner with a tmpfs; it no longer fits the files and would pass by seeing nothing")
	}
}
