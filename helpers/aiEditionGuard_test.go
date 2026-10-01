package helpers

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// OneCamp is sold as TWO EDITIONS, and every licence includes both:
//
//	v2  OneCamp with the AI teammates.
//	v1  OneCamp with no AI at all, for organisations whose policy does not permit
//	    sending their data to a model.
//
// v1 is produced by removing the AI surface. That only stays affordable while the
// surface has a THIN BOUNDARY: the fewer non-AI packages that reach into AI code, the
// fewer call sites have to be stubbed to make a build without it. Nothing in the compiler
// or the test suite noticed the boundary widening, so it widened quietly, and the AI-free
// branch became a hand-curated cherry-pick across several hundred commits — which is
// exactly how AI code ends up in the edition sold as AI-free.
//
// These tests make the boundary a recorded fact, so widening it is a visible decision
// taken once rather than an accident discovered at release time.

// aiDomainNames are the package-name components that place a package inside the AI
// surface.
//
// Matched as a path SEGMENT rather than as a listed set of packages, so a new
// business/AIWhatever is AI surface from the moment it is created and needs no
// bookkeeping here. Listing packages by hand is what let domain/AIAgent, domain/CodePR
// and models/postgres/AIMCP look like boundary crossings when they are AI code itself.
var aiDomainNames = map[string]bool{
	"AI":         true,
	"AIAgent":    true,
	"AICoworker": true,
	"AIMCP":      true,
	"MCP":        true, // Model Context Protocol: an AI integration surface.
	"MCPServer":  true,
	"CodeAgent":  true,
	"CodePR":     true,
	"AIDrill":    true, // the governance drill: it drives an agent executor, so v1 has nothing to run.
	"aieval":     true, // cmd/aieval, the agent evaluation harness.
}

// aiEditionBoundary is every non-AI package that imports AI code, and therefore every
// place the AI-free build has to be carved.
//
// A FAILURE HERE IS NOT A BUG, it is a bill. Adding an entry means the AI-free edition
// gained another call site to stub, so the entry belongs in this list along with a
// deliberate decision to pay for it. Prefer, in order:
//
//  1. Put the AI call behind an interface the non-AI build can satisfy with a no-op.
//  2. Move the AI-aware code into an AI package and have the non-AI side publish an
//     event the AI package subscribes to.
//  3. Accept the crossing and record it here.
//
// Keyed on PACKAGES, not files: a second file in a package that already crosses the
// boundary costs the carve nothing extra, and a guard that fires on it would be noise.
var aiEditionBoundary = map[string]bool{
	"business/ApiToken":     true,
	"business/Archive":      true,
	"business/Board":        true,
	"business/Channel":      true,
	"business/Chat":         true,
	"business/Command":      true,
	"business/Comment":      true,
	"business/DataTable":    true,
	"business/Doc":          true,
	"business/Marketplace":  true,
	"business/Post":         true,
	"business/Project":      true,
	"business/Task":         true,
	"business/Workflow":     true,
	"cmd/server":            true,
	"controllers/Board":     true,
	"controllers/Channel":   true,
	"controllers/DataTable": true,
	"controllers/LiveKit":   true,
	"controllers/User":      true,
	"controllers/V1":        true,
	"router":                true,
	// The integration suite exercises agents and MCP end to end. It is a real crossing:
	// the AI-free build cannot compile these tests, so the carve has to drop them, and
	// dropping tests is worth noticing rather than discovering.
	"tests/integration": true,
}

const oneCampModule = "github.com/akashc777/OneCamp/"

// isAIPackage reports whether a repo-relative package path is part of the AI surface.
func isAIPackage(pkgPath string) bool {
	for _, segment := range strings.Split(pkgPath, "/") {
		if aiDomainNames[segment] {
			return true
		}
	}
	return false
}

// aiImportCrossings walks the repository and returns, for every non-AI package, the AI
// packages it imports.
func aiImportCrossings(t *testing.T) map[string]map[string]bool {
	t.Helper()

	crossings := map[string]map[string]bool{}
	fset := token.NewFileSet()

	err := filepath.Walk("..", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Vendored and generated trees are not ours to carve, and data/ is a
			// runtime volume that the test process cannot always read.
			switch info.Name() {
			case "vendor", "node_modules", ".git", "data", "sdk", "other-services":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		pkgPath := filepath.ToSlash(filepath.Dir(strings.TrimPrefix(filepath.ToSlash(path), "../")))
		if pkgPath == "." || isAIPackage(pkgPath) {
			return nil
		}

		file, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			// A file that does not parse is a compile error the build reports better
			// than this test would.
			return nil
		}

		for _, imp := range file.Imports {
			target := strings.Trim(imp.Path.Value, `"`)
			if !strings.HasPrefix(target, oneCampModule) {
				continue
			}
			target = strings.TrimPrefix(target, oneCampModule)
			if !isAIPackage(target) {
				continue
			}
			if crossings[pkgPath] == nil {
				crossings[pkgPath] = map[string]bool{}
			}
			crossings[pkgPath][target] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}
	return crossings
}

// TestAIEditionBoundaryIsRecorded fails when a package starts or stops importing AI code
// without the list above being updated to match.
func TestAIEditionBoundaryIsRecorded(t *testing.T) {
	crossings := aiImportCrossings(t)

	if len(crossings) == 0 {
		t.Fatal("found no packages importing AI code at all. The AI surface is large and " +
			"the router wires it, so this means the walk or the module prefix is wrong " +
			"rather than that the boundary is clean.")
	}

	var unrecorded []string
	for pkg, targets := range crossings {
		if aiEditionBoundary[pkg] {
			continue
		}
		names := make([]string, 0, len(targets))
		for target := range targets {
			names = append(names, target)
		}
		sort.Strings(names)
		unrecorded = append(unrecorded, pkg+" -> "+strings.Join(names, ", "))
	}
	sort.Strings(unrecorded)

	if len(unrecorded) > 0 {
		t.Errorf("%d package(s) import AI code but are not recorded in aiEditionBoundary:\n  %s\n\n"+
			"Each one is a call site the AI-free v1 edition has to be carved around, and v1 "+
			"is sold to customers whose policy forbids AI. Either route the call through an "+
			"interface the non-AI build can satisfy with a no-op, or add the package to "+
			"aiEditionBoundary to record that the carve just got more expensive.",
			len(unrecorded), strings.Join(unrecorded, "\n  "))
	}

	var departed []string
	for pkg := range aiEditionBoundary {
		if len(crossings[pkg]) == 0 {
			departed = append(departed, pkg)
		}
	}
	sort.Strings(departed)

	if len(departed) > 0 {
		t.Errorf("%d package(s) are recorded in aiEditionBoundary but no longer import AI "+
			"code:\n  %s\n\nThat is progress: remove them from the list so it keeps "+
			"describing the real cost of building v1.",
			len(departed), strings.Join(departed, "\n  "))
	}
}

// aiOnlyEnvKeys configure the AI edition and nothing else, so the AI-free edition's
// template should not carry them. Their absence must not affect a v1 build.
var aiOnlyEnvKeys = []string{
	"AI_ENABLED",
	"AI_PROVIDER",
	"AI_RATE_LIMIT_PER_MIN",
	"AI_CONFIG_KEK",
	"AI_DISK_PATH",
	"AI_WORKSPACE_DAILY_TOKEN_BUDGET",
	"AI_USER_DAILY_TOKEN_BUDGET",
	"OLLAMA_HOST",
	"OLLAMA_MODEL",
	"OLLAMA_EMBEDDING_MODEL",
	"OLLAMA_IMAGE_TAG",
	"OPENAI_MODEL",
	"OPENAI_EMBEDDING_MODEL",
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_MODEL",
}

// sharedEnvKeysThatLookAI read as AI configuration by NAME but are consumed by features
// that have nothing to do with the AI teammates. Removing them to build v1 would break
// the feature named in the reason.
//
// This list is the whole reason these tests exist. A name-based strip of everything
// matching AI|OPENAI|OLLAMA looks obviously correct and would ship a v1 with speech-to-
// text transcription broken, because transcription accepts an OpenAI-compatible endpoint
// for Whisper and reads the same key the AI provider does.
var sharedEnvKeysThatLookAI = map[string]string{
	"OPENAI_API_KEY":    "business/Transcription: speech-to-text for meeting recordings, which is a meetings feature and not an AI teammate",
	"CODE_RUNNER_TOKEN": "other-services/code-runner: a standalone sandbox service with its own binary, which validates this token itself at startup",
	"AI_DATASOURCE_HOST_ALLOWLIST": "business/DataSource/hostGuard.go: the operator's SSRF allowlist for external database connections. " +
		"business/DataSource imports no AI code, so it survives into v1, and the name is the only AI thing about this key. " +
		"Removing it would not open the dangerous case (internal and cloud-metadata addresses are refused whether or not " +
		"an allowlist is set) but it WOULD silently remove the operator's ability to restrict which public hosts a data " +
		"source may reach.",
}

// envTemplatesForEditionCheck are the templates a customer actually configures.
func envTemplatesForEditionCheck(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob("../vars/.env.*")
	if err != nil {
		t.Fatalf("globbing env templates: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("found no ../vars/.env.* templates; this guard would silently pass forever")
	}
	return paths
}

// TestEveryAILookingEnvKeyIsClassified requires each AI-looking key in the shipped
// templates to be recorded as either AI-only or shared-with-a-reason.
//
// Without this, producing the v1 template is a judgement call made once per release by
// whoever is doing the merge, against a 500-line file. With it, the decision is recorded
// next to the key and reviewed when it changes.
func TestEveryAILookingEnvKeyIsClassified(t *testing.T) {
	aiOnly := map[string]bool{}
	for _, key := range aiOnlyEnvKeys {
		aiOnly[key] = true
	}

	// A key is "AI-looking" if its name carries one of these, which is exactly the test
	// a person doing the strip by hand would apply.
	markers := []string{"AI_", "_AI", "OLLAMA", "OPENAI", "ANTHROPIC", "GEMINI", "LLM", "EMBEDDING"}

	found := map[string][]string{} // key -> templates it appears in
	for _, path := range envTemplatesForEditionCheck(t) {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for _, line := range strings.Split(string(content), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			eq := strings.Index(trimmed, "=")
			if eq <= 0 {
				continue
			}
			key := strings.TrimSpace(trimmed[:eq])
			looksAI := false
			for _, marker := range markers {
				if strings.Contains(key, marker) {
					looksAI = true
					break
				}
			}
			if !looksAI {
				continue
			}
			found[key] = append(found[key], filepath.Base(path))
		}
	}

	if len(found) == 0 {
		t.Fatal("found no AI-looking keys in any template. The AI edition is configured " +
			"through these files, so this means the parser is wrong rather than that " +
			"there is nothing to classify.")
	}

	var unclassified []string
	for key, templates := range found {
		if aiOnly[key] {
			continue
		}
		if _, ok := sharedEnvKeysThatLookAI[key]; ok {
			continue
		}
		sort.Strings(templates)
		unclassified = append(unclassified, key+" (in "+strings.Join(templates, ", ")+")")
	}
	sort.Strings(unclassified)

	if len(unclassified) > 0 {
		t.Errorf("%d AI-looking config key(s) are not classified:\n  %s\n\n"+
			"Building the AI-free v1 edition means deciding, for every one of these, "+
			"whether it goes or stays. Add each to aiOnlyEnvKeys, or to "+
			"sharedEnvKeysThatLookAI with the non-AI feature that reads it.",
			len(unclassified), strings.Join(unclassified, "\n  "))
	}
}

// TestSharedAIEnvKeysReallyHaveANonAIReader is the negative check on the exception list.
//
// sharedEnvKeysThatLookAI is the dangerous half of the classification: every entry is a
// key that will SURVIVE into the AI-free edition. Left unverified it becomes a place to
// silence the other test, and then v1 ships carrying AI configuration after all. So each
// entry has to be justified by real code outside the AI surface reading it.
func TestSharedAIEnvKeysReallyHaveANonAIReader(t *testing.T) {
	if len(sharedEnvKeysThatLookAI) == 0 {
		t.Skip("no shared keys claimed")
	}

	readers := map[string][]string{}
	fset := token.NewFileSet()

	err := filepath.Walk("..", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case "vendor", "node_modules", ".git", "data", "sdk":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel := strings.TrimPrefix(filepath.ToSlash(path), "../")
		pkgPath := filepath.ToSlash(filepath.Dir(rel))
		if isAIPackage(pkgPath) {
			return nil
		}
		// This guard file names the keys in its own documentation.
		if strings.HasSuffix(rel, "aiEditionGuard_test.go") {
			return nil
		}

		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		for key := range sharedEnvKeysThatLookAI {
			if strings.Contains(string(content), `"`+key+`"`) {
				readers[key] = append(readers[key], rel)
			}
		}
		_ = fset
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}

	for key, reason := range sharedEnvKeysThatLookAI {
		if len(readers[key]) == 0 {
			t.Errorf("%s is recorded as shared because %q, but no file outside the AI "+
				"surface reads it. Either the reason is stale and the key is AI-only "+
				"(move it to aiOnlyEnvKeys, so v1 stops carrying it), or the reader was "+
				"removed.", key, reason)
			continue
		}
		sort.Strings(readers[key])
		t.Logf("%s is read outside the AI surface by: %s", key, strings.Join(readers[key], ", "))
	}
}
