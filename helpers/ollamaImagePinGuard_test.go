package helpers

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The Ollama engine image must be pinned to one explicit version, and every place that names it must
// name the same one.
//
// WHY THIS IS A TEST AND NOT A CONVENTION. The admin panel reads the running engine version, compares
// it against the newest published release, and shows "update available" with a way to act on it. Every
// part of that is a lie against a floating tag:
//
//   - `latest` means the deployed version is whatever the tag pointed at the day the image was first
//     pulled. Two hosts installed a month apart run different engines and both report a clean deploy.
//   - There is nothing to roll back to. "Undo the update" has no referent.
//   - A `docker compose pull` aimed at some other service can move the engine underneath a running
//     workspace, because compose resolves the tag again.
//
// It was `ollama/ollama:latest` in three files while the panel offered the update button, and the beta
// host was running 0.21.2 against a `latest` that had moved on to 0.32.9 — an eleven-minor gap that a
// routine recreate would have crossed silently, mid-incident, with no way back.
//
// THE SKEW HALF IS SEPARATE AND WORSE. ollama-init is the same image used as a CLI against the server,
// so the two entries in one file must agree; and every compose file naming the image must agree with
// every other, or the same product ships two different engines depending on topology. Nothing about a
// mismatched pair looks wrong in review — both lines are pinned, which is the property a reader checks
// for.
//
// The check globs rather than listing files. That is the opposite of the choice in
// composeRestartGuard_test.go, deliberately: there the claim is about a known set of deployed stacks,
// so a glob finding nothing would silently assert nothing. Here the claim is "wherever this image is
// named, it is pinned", which has to cover a compose file nobody has written yet. The reference floor
// below is what keeps a glob that matches nothing from passing.
const (
	// ollamaTagVar is the single knob. Named once here so the failure messages and the env-file check
	// cannot drift from the pattern.
	ollamaTagVar = "OLLAMA_IMAGE_TAG"

	// minOllamaImageRefs is the "am I scanning anything" floor. Four references exist today (server and
	// init, in each of two stacks). Two is low enough to survive a stack legitimately dropping the
	// engine, and high enough that a pattern which has stopped matching fails here instead of passing
	// vacuously.
	minOllamaImageRefs = 2
)

var (
	// Matches the image reference and captures everything after the colon, so a bare `latest` is caught
	// by the same expression that validates the pinned form.
	ollamaImageRef = regexp.MustCompile(`ollama/ollama:(\S+)`)

	// The one accepted shape: ${OLLAMA_IMAGE_TAG:-<default>}. The default is what gets asserted.
	ollamaPinnedForm = regexp.MustCompile(`^\$\{` + ollamaTagVar + `:-([^}]+)\}$`)

	// A plain release version. Ollama publishes tags without the leading "v"; requiring that shape also
	// rejects `latest`, `rocm`, and the `-rc0` prereleases, none of which belong in a deployment.
	ollamaPlainVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

	// KEY=value in an env file, anchored so a line merely containing the key does not count.
	ollamaTagAssignment = regexp.MustCompile(`^` + ollamaTagVar + `=(.*)$`)
)

// ollamaEnvFiles are the env files whose declared tag must agree with the compose default. Listed
// explicitly, because unlike the compose glob this is a claim about specific known files: these are the
// ones a deployment is actually started with. shared.env was the third and was deleted with the
// multi-tenant stack it configured.
var ollamaEnvFiles = []string{
	"../vars/.env.beta",
	"../vars/.env.prod",
}

// ollamaImageSite is one place the image is named, kept with its location so a failure says which line
// to open rather than which property was violated.
type ollamaImageSite struct {
	file string
	line int
	ref  string
}

// ollamaImageSites returns every reference to the Ollama image across the repository's compose files.
func ollamaImageSites(t *testing.T) []ollamaImageSite {
	t.Helper()

	paths, err := filepath.Glob("../*compose*.yml")
	if err != nil {
		t.Fatalf("globbing compose files: %v", err)
	}
	sort.Strings(paths)

	var sites []ollamaImageSite
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			// Skip comments: this very file's rationale is quoted in the compose headers, and a comment
			// mentioning the image is not a deployment instruction.
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if m := ollamaImageRef.FindStringSubmatch(line); m != nil {
				sites = append(sites, ollamaImageSite{file: filepath.Base(path), line: i + 1, ref: m[1]})
			}
		}
	}

	if len(sites) < minOllamaImageRefs {
		t.Fatalf("found %d Ollama image reference(s) across ../*compose*.yml, expected at least %d. "+
			"Either the image moved out of compose or this pattern no longer matches it; a check that "+
			"scans nothing passes for the wrong reason", len(sites), minOllamaImageRefs)
	}
	return sites
}

func TestOllamaImageIsPinned(t *testing.T) {
	var problems []string
	for _, s := range ollamaImageSites(t) {
		m := ollamaPinnedForm.FindStringSubmatch(s.ref)
		if m == nil {
			problems = append(problems, fmt.Sprintf(
				"%s:%d names ollama/ollama:%s. Use ollama/ollama:${%s:-<version>} so the deployed engine "+
					"is a version somebody chose and can put back",
				s.file, s.line, s.ref, ollamaTagVar))
			continue
		}
		if !ollamaPlainVersion.MatchString(m[1]) {
			problems = append(problems, fmt.Sprintf(
				"%s:%d defaults %s to %q. It must be a plain published release like 0.21.2 — no leading "+
					"\"v\", no `latest`, no -rc prerelease, no -rocm variant",
				s.file, s.line, ollamaTagVar, m[1]))
		}
	}

	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("the admin panel reports the engine version and offers to update it, which needs a "+
			"pinned tag to mean anything:\n  %s", strings.Join(problems, "\n  "))
	}
}

func TestOllamaImageTagIsConsistentEverywhere(t *testing.T) {
	// default value -> the places that declare it, so the failure can name both sides of a skew.
	byDefault := map[string][]string{}

	for _, s := range ollamaImageSites(t) {
		m := ollamaPinnedForm.FindStringSubmatch(s.ref)
		if m == nil {
			// Already reported by TestOllamaImageIsPinned; nothing to compare.
			continue
		}
		byDefault[m[1]] = append(byDefault[m[1]], fmt.Sprintf("%s:%d", s.file, s.line))
	}

	for _, path := range ollamaEnvFiles {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("cannot read %s: %v. If an env file moved, update ollamaEnvFiles — the tag it "+
				"declares overrides the compose default, so an unchecked one is the value that "+
				"actually deploys", path, err)
		}
		found := false
		for i, line := range strings.Split(string(raw), "\n") {
			m := ollamaTagAssignment.FindStringSubmatch(strings.TrimSpace(line))
			if m == nil {
				continue
			}
			found = true
			byDefault[strings.TrimSpace(m[1])] = append(
				byDefault[strings.TrimSpace(m[1])], fmt.Sprintf("%s:%d", filepath.Base(path), i+1))
		}
		if !found {
			t.Errorf("%s does not declare %s. The compose default would apply, which works but leaves "+
				"the deployed engine version absent from the file an operator reads to find it",
				filepath.Base(path), ollamaTagVar)
		}
	}

	if len(byDefault) <= 1 {
		return
	}

	versions := make([]string, 0, len(byDefault))
	for v := range byDefault {
		versions = append(versions, v)
	}
	sort.Strings(versions)

	var detail []string
	for _, v := range versions {
		sites := byDefault[v]
		sort.Strings(sites)
		detail = append(detail, fmt.Sprintf("%s <- %s", v, strings.Join(sites, ", ")))
	}

	t.Errorf("%s resolves to %d different versions, so what runs depends on which file was read:\n  %s\n"+
		"ollama-init is the same image used as a CLI against the server, and the compose files are the "+
		"same product on different topologies. All of them have to agree.",
		ollamaTagVar, len(byDefault), strings.Join(detail, "\n  "))
}
