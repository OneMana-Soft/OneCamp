package helpers

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A required-variable marker, ${VAR:?message}, refuses to start ANYTHING when VAR is
// unset. Compose evaluates interpolation across the WHOLE FILE before it decides which
// services to run, so the marker does not care which profiles are active.
//
// That makes `${VAR:?}` on a profile-gated service a trap: the service never starts
// unless someone asks for it, yet its variable blocks every operator who never will.
// CODE_RUNNER_TOKEN did exactly this. The optional code-execution sandbox sits behind
// `profiles: ["code-execution"]`, and its token was marked required, so
// `make build_restart_all` stopped with
//
//	required variable CODE_RUNNER_TOKEN is missing a value
//
// before any service started, on a first install, for a sandbox the operator had not
// asked for and might never use.
//
// The rule this encodes: A REQUIRED MARKER MAY ONLY GUARD A SERVICE THAT ALWAYS RUNS.
// If a service is optional, its configuration has to be optional too, and the service
// itself is the right place to refuse to start — it is the only participant that knows
// whether the value actually matters.

var (
	// requiredMarker finds ${NAME:?...}, the "refuse to start" form.
	requiredMarker = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*):\?`)
	// serviceKey matches a service name at exactly two spaces of indentation.
	// Trailing comments are allowed: `alpha: # dgraph` is a service key, and a
	// previous guard missed one written that way.
	serviceKey = regexp.MustCompile(`^  ([A-Za-z0-9_.-]+):\s*(#.*)?$`)
	// profilesKey matches a `profiles:` entry inside a service block.
	profilesKey = regexp.MustCompile(`^\s+profiles:`)
)

// composeFilesForRequiredVarCheck returns the compose files in the repository.
//
// Globbed rather than listed: the claim is about wherever a required marker appears, and
// a hand-written list of compose files has already rotted once in this repository.
func composeFilesForRequiredVarCheck(t *testing.T) []string {
	t.Helper()

	paths, err := filepath.Glob("../*compose*.yml")
	if err != nil {
		t.Fatalf("globbing compose files: %v", err)
	}
	if len(paths) < 3 {
		t.Fatalf("found %d compose file(s); expected several. A glob that matches nothing "+
			"makes this guard pass forever.", len(paths))
	}
	sort.Strings(paths)
	return paths
}

// profileGatedServices returns the services in one compose file that sit behind a
// profile, and therefore do not start unless explicitly requested.
func profileGatedServices(content string) map[string]bool {
	gated := map[string]bool{}

	var current string
	inServices := false
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "services:") {
			inServices = true
			continue
		}
		// Any other top-level key ends the services block.
		if inServices && len(line) > 0 && line[0] != ' ' && line[0] != '#' {
			inServices = false
		}
		if !inServices {
			continue
		}
		if m := serviceKey.FindStringSubmatch(line); m != nil {
			current = m[1]
			continue
		}
		if current != "" && profilesKey.MatchString(line) {
			gated[current] = true
		}
	}
	return gated
}

// stripYAMLComment removes the comment portion of a line, leaving configuration only.
//
// Necessary, not tidiness. The first version of this guard scanned raw lines and fired on
// the sentence "NOT a required-variable marker (${VAR:?})" written in a comment
// explaining why the marker had been removed — reporting a variable literally named VAR
// in three files. A guard that reads prose as configuration reports faults that are not
// there, and one that has cried wolf gets deleted.
//
// A `#` inside a quoted value is not a comment, so quotes are tracked.
func stripYAMLComment(line string) string {
	inSingle, inDouble := false, false
	for i, r := range line {
		switch r {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble {
				return line[:i]
			}
		}
	}
	return line
}

// serviceOfLine reports which service a given line index belongs to.
func serviceOfLine(lines []string, target int) string {
	for i := target; i >= 0; i-- {
		if m := serviceKey.FindStringSubmatch(lines[i]); m != nil {
			return m[1]
		}
	}
	return ""
}

// TestNoRequiredVariableGuardsAnOptionalService is the rule.
func TestNoRequiredVariableGuardsAnOptionalService(t *testing.T) {
	// A compose file whose whole purpose is one optional service is exempt: the service
	// always runs when that file is used, so requiring its configuration is correct.
	dedicatedFiles := map[string]bool{
		"code-runner-coding-compose.yml": true,
	}

	checked := 0
	var offences []string

	for _, path := range composeFilesForRequiredVarCheck(t) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		content := string(raw)
		base := filepath.Base(path)
		if dedicatedFiles[base] {
			continue
		}

		gated := profileGatedServices(content)
		if len(gated) == 0 {
			continue
		}

		lines := strings.Split(content, "\n")
		for i, line := range lines {
			for _, m := range requiredMarker.FindAllStringSubmatch(stripYAMLComment(line), -1) {
				checked++
				svc := serviceOfLine(lines, i)
				if gated[svc] {
					offences = append(offences, base+":"+svc+" requires "+m[1])
				}
			}
		}
	}

	if len(offences) > 0 {
		sort.Strings(offences)
		t.Errorf("%d required variable(s) guard a service that only runs behind a profile:\n  %s\n\n"+
			"Compose evaluates ${VAR:?} across the whole file before it picks which services "+
			"to start, so this refuses to start ANYTHING for every operator who never enables "+
			"the optional service. Use ${VAR:-} and let the service itself refuse to start "+
			"without a value; it is the only one that knows whether the value matters.",
			len(offences), strings.Join(offences, "\n  "))
	}
}

// TestRequiredVariablesAreStillUsed keeps the guard above honest.
//
// If the marker form disappeared from every compose file, the test above would pass by
// having nothing to inspect. The Traefik values are genuinely required — the proxy runs
// in every deployment and cannot serve TLS without them — so at least one marker should
// always exist.
func TestRequiredVariablesAreStillUsed(t *testing.T) {
	total := 0
	for _, path := range composeFilesForRequiredVarCheck(t) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			total += len(requiredMarker.FindAllString(stripYAMLComment(line), -1))
		}
	}

	if total == 0 {
		t.Error("no ${VAR:?} markers anywhere. Either they were all removed, in which case " +
			"nothing now refuses to start on missing TLS configuration, or the pattern no " +
			"longer matches and TestNoRequiredVariableGuardsAnOptionalService is inspecting " +
			"nothing.")
	}
}

// TestOptionalServiceConfigIsNotAPlaceholder closes the other half of the same problem.
//
// `make doctor` refuses to proceed while any __CHANGE_ME_*__ placeholder remains, which is
// right for values every install needs and wrong for a value only an optional service
// wants: it moves the same blockage from Compose into the Makefile. So configuration for
// a profile-gated service must ship EMPTY, not as a placeholder.
func TestOptionalServiceConfigIsNotAPlaceholder(t *testing.T) {
	// Keys belonging only to profile-gated services, with the service that owns each.
	optionalServiceKeys := map[string]string{
		"CODE_RUNNER_TOKEN": "code-runner, behind profiles: [code-execution]",
	}

	paths, err := filepath.Glob("../vars/.env.*")
	if err != nil {
		t.Fatalf("globbing env templates: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("found no ../vars/.env.* templates")
	}

	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			eq := strings.Index(trimmed, "=")
			if eq <= 0 {
				continue
			}
			key := strings.TrimSpace(trimmed[:eq])
			owner, optional := optionalServiceKeys[key]
			if !optional {
				continue
			}
			value := strings.TrimSpace(trimmed[eq+1:])
			if strings.Contains(value, "__CHANGE_ME") {
				t.Errorf("%s sets %s to a placeholder, but it configures %s. "+
					"`make doctor` refuses to proceed while a placeholder remains, so every "+
					"operator is forced to invent a secret for a service they may never start. "+
					"Ship it empty and let the service refuse to start without it.",
					filepath.Base(path), key, owner)
			}
		}
	}
}
