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

// Every docker compose invocation in the Makefile must name files that exist and services those files
// declare.
//
// WHY. The Makefile is the deployment interface, and nothing checks it. Two faults in it were found by
// reading rather than by any test:
//
//   - Three collaboration-service targets pointed at hocuspocus-compose.yml, a 2025 local-dev stub that
//     declared the service WITHOUT the Redis password, the traefik labels or the restart policy that the
//     deployed definition in final-compose.yml has. Running the target named after deploying that
//     service would have replaced the working container with an unreachable one and silently undone the
//     restart policy from b91fc42.
//   - There was no rebuild target for the main stack at all, so the most frequent operation on a live
//     deployment was a compose line typed from memory.
//
// This does not catch a stale-but-present definition — that was fixed by deleting the duplicate, since
// two definitions of one service is the fault itself. What it does catch is the mechanical half: a
// target naming a file that has been moved or deleted, or a service that its compose file does not
// declare. Both are silent until someone runs the target, in an operational moment, on a real host.
//
// It also protects the deletion just made: had the stub been removed while a target still referenced it,
// this test would have failed instead of leaving `make build_collaboration_service` broken.

var (
	composeFileFlag = regexp.MustCompile(`-f\s+([A-Za-z0-9._/-]+\.yml)`)
	// A compose service key: exactly two spaces of indentation, then a name, then a colon, and
	// optionally a trailing comment. The comment branch is load-bearing: without it this pattern skips
	// `opensearch-node1: # ...` in distribute-compose.yml, and a Makefile target naming that service
	// would then be reported as naming a service its compose file does not declare. Same bug as the one
	// documented in TestComposeParserHasNoBlindSpots, in a second copy of the same idea.
	composeServiceKey = regexp.MustCompile(`^  ([a-z0-9][a-z0-9._-]*):\s*(?:#.*)?$`)
	// A plain service name on a command line. Anything with a $, /, = or . is a variable, path or flag
	// value and is skipped rather than guessed at.
	plainServiceName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
)

// composeSubcommands maps a subcommand to how many of the trailing non-flag tokens are service names.
// -1 means "all of them". exec takes ONE service and then a command to run inside it, so only the first
// token may be checked; treating the rest as services would report `sh` and `psql` as missing.
var composeSubcommands = map[string]int{
	"up": -1, "down": -1, "stop": -1, "start": -1, "restart": -1,
	"logs": -1, "ps": -1, "pull": -1, "build": -1,
	"exec": 1,
}

// composeFlagsTakingValue are flags whose following token is a value, not a service.
var composeFlagsTakingValue = map[string]bool{
	"--env-file": true, "--project-name": true, "-p": true, "-f": true,
	"--profile": true, "--tail": true, "--scale": true, "--index": true,
	"--workdir": true, "-w": true, "-u": true, "--user": true, "-e": true,
}

// servicesIn returns the service names a compose file declares.
func servicesIn(t *testing.T, path string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	out := map[string]bool{}
	inServices := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "services:") {
			inServices = true
			continue
		}
		// A new top-level key ends the services block: networks:, volumes:, and so on.
		if inServices && len(line) > 0 && line[0] != ' ' && line[0] != '#' {
			inServices = false
		}
		if !inServices {
			continue
		}
		if m := composeServiceKey.FindStringSubmatch(line); m != nil {
			out[m[1]] = true
		}
	}
	return out
}

// composeLines returns the Makefile lines that invoke docker compose, joined across backslash
// continuations so a service named on a wrapped line is still seen.
func composeLines(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatalf("cannot read ../Makefile: %v", err)
	}
	joined := strings.ReplaceAll(string(raw), "\\\n", " ")
	var out []string
	for _, line := range strings.Split(joined, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(line, "docker compose") {
			out = append(out, line)
		}
	}
	if len(out) < 10 {
		t.Fatalf("found only %d docker compose lines; the Makefile has changed shape and this check "+
			"is no longer inspecting it", len(out))
	}
	return out
}

func TestMakefileComposeFilesExist(t *testing.T) {
	seen := map[string]bool{}
	for _, line := range composeLines(t) {
		for _, m := range composeFileFlag.FindAllStringSubmatch(line, -1) {
			seen[m[1]] = true
		}
	}
	if len(seen) == 0 {
		t.Fatal("matched no -f arguments at all; the pattern no longer fits the Makefile")
	}

	var missing []string
	for f := range seen {
		if _, err := os.Stat(filepath.Join("..", f)); err != nil {
			missing = append(missing, f)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the Makefile names %d compose file(s) that do not exist, so the targets using them "+
			"fail the moment somebody runs them:\n  %s", len(missing), strings.Join(missing, "\n  "))
	}
}

func TestMakefileComposeServicesAreDeclared(t *testing.T) {
	// Cache, since final-compose.yml is named by most lines.
	cache := map[string]map[string]bool{}
	declared := func(f string) map[string]bool {
		if s, ok := cache[f]; ok {
			return s
		}
		p := filepath.Join("..", f)
		if _, err := os.Stat(p); err != nil {
			cache[f] = map[string]bool{}
			return cache[f]
		}
		cache[f] = servicesIn(t, p)
		return cache[f]
	}

	var problems []string
	checked := 0

	for _, line := range composeLines(t) {
		files := composeFileFlag.FindAllStringSubmatch(line, -1)
		if len(files) == 0 {
			continue
		}
		known := map[string]bool{}
		var names []string
		for _, m := range files {
			names = append(names, m[1])
			for s := range declared(m[1]) {
				known[s] = true
			}
		}

		// Everything after the subcommand, stopping at a shell operator: the tokens beyond && belong to
		// another command entirely.
		fields := strings.Fields(line)
		subIdx, allowance := -1, 0
		for i, f := range fields {
			if n, ok := composeSubcommands[f]; ok && i > 0 && fields[i-1] != "-f" {
				subIdx, allowance = i, n
				break
			}
		}
		if subIdx < 0 {
			continue
		}

		found := 0
		for i := subIdx + 1; i < len(fields); i++ {
			tok := fields[i]
			if tok == "&&" || tok == "||" || tok == ";" || tok == "|" {
				break
			}
			if strings.HasPrefix(tok, "-") {
				if composeFlagsTakingValue[tok] {
					i++
				}
				continue
			}
			if !plainServiceName.MatchString(tok) {
				continue
			}
			if allowance >= 0 && found >= allowance {
				break
			}
			found++
			checked++
			if !known[tok] {
				problems = append(problems, fmt.Sprintf("%q is not declared in %s",
					tok, strings.Join(names, " or ")))
			}
		}
	}

	if checked == 0 {
		t.Fatal("checked no service names; the parser no longer recognises the Makefile's compose lines")
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("%d Makefile target(s) name a service their compose file does not declare. Either the "+
			"target points at the wrong file, or the service was renamed:\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}
