package ai

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"unicode"
)

// An unexported function that only its own test calls is not covered — it is unreachable.
//
// WHY THIS EXISTS. rescueTarget was written to parse the context limit a provider states
// when it refuses a prompt, unit-tested against every message shape, and never called.
// ChatWithRescue kept its earlier inline computation, so the parsing ran in tests and
// nowhere else. Everything was green: the helper's tests passed because the helper was
// correct, and the end-to-end tests passed because the old path still worked well enough
// on the fixtures. A whole mechanism shipped as decoration.
//
// go vet does not report this. The two linters installed here are built against older Go
// releases than this module requires, so neither can analyse it. Rather than add tooling
// that cannot be verified, this walks the source directly — the same class of check,
// inside the test suite that already runs.
//
// SCOPE AND FALSE POSITIVES. Only plain unexported functions: a method may be reached
// through an interface, so absence of a direct reference proves nothing about it. Init
// hooks and constructors registered indirectly are the other legitimate case, and so is
// finished work that is not wired up yet. All of them declare it with a local marker so
// the reason sits at the declaration.
//
// The marker is NOT an escape hatch for "I could not be bothered". It is the difference
// between a mechanism that silently does nothing and a deliberate decision someone can
// read: a prepared optimisation nobody has switched on is fine and should say so; a
// parser wired to nothing is the bug this exists to catch.
//
// HOW IT DECIDES. A reachability walk, not a reference tally. Roots are everything that
// can be entered from outside the package — exported functions, any method (reachable
// through an interface), init, main — plus every package-level var and const initialiser,
// which runs regardless. An unexported function is alive only if some root reaches it
// through a chain of calls.
//
// It counted direct references until it was upgraded, and that had a limitation worth
// recording because it was observed rather than guessed: a CLUSTER of dead functions hid
// itself. A dead helper calling a second dead helper gave the second one a reference, so
// only the first was reported — a dead sanitizeFileName kept its own sanitizeString
// invisible exactly that way. A walk has no such blind spot: neither member of a mutually
// recursive pair is reachable from a root, so both are reported.
//
// REMAINING LIMITATION. Reachability here is within a package, which is the right scope
// for unexported functions since nothing outside can name them, but the roots are trusted:
// an exported function that is itself never called anywhere keeps everything below it
// alive. Finding those needs a cross-package graph over exported names. So a pass means no
// unexported function is orphaned; it does not mean every exported one is used.
const deadHelperExemptMarker = "unreachable-by-design:"

func TestNoUnexportedFunctionIsReachableOnlyFromTests(t *testing.T) {
	// WHOLE MODULE. This started as a hand-picked list, because a ratchet that fails on a
	// dozen unreviewed findings is a ratchet somebody disables, and each package was reviewed
	// and cleared before its root was added. Running the same scan across every directory then
	// showed the backlog was down to a single finding (helpers getLoggerForLevel, a pre-slog
	// level-router referenced by nothing, since deleted), so there is no longer a reason to
	// enumerate packages: new code in ANY package is covered from the moment it is written,
	// which is the point.
	//
	// Excluded, deliberately:
	//   other-services/code-runner  a separate Go module, not built by this one.
	//   tests/                      integration harness code. The premise here is "declared in
	//                               production code"; a harness helper used only by tests is
	//                               correct, not dead.
	//
	// The packages that were adopted one at a time each earned it by being reviewed rather
	// than swept, and the findings are worth keeping on record:
	//   GitHub          getGitHubBotLogin was not stale code, it was the missing half of a
	//                   live loop guard, and is now wired to it.
	//   BulkPostAndChat buildGrpName was genuinely superseded — but reviewing WHY led to the
	//                   positional slice pairing that misindexed forwarded messages into the
	//                   wrong conversation.
	//   Command         inChannel was a redundant wrapper; the in_channel capability it
	//                   implied is live in poll.go, which builds the response directly.
	//   MCPServer       resetRegistryForTest was a real test fixture sitting in production
	//                   code; it moved to a _test.go file, which is both where it belongs and
	//                   what stops this check flagging it.
	//   Auth            provisionAndRedirect was a dead no-groups wrapper around the live SSO
	//                   provisioning helper — deleted, because a friendlier name that skips
	//                   admin group reconciliation is a footgun in an auth path.
	roots := []string{
		"../../adapter",
		"../../business",
		"../../cmd",
		"../../controllers",
		"../../domain",
		"../../helpers",
		"../../initializers",
		"../../middleware",
		"../../models",
		"../../router",
		"../../services",
	}

	type decl struct {
		file string
		line int
	}

	var offenders []string
	pkgDirs := map[string]bool{}
	for _, root := range roots {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("scan root %s missing — this ratchet would silently pass: %v", root, err)
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if strings.HasSuffix(path, ".go") {
				pkgDirs[filepath.Dir(path)] = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(pkgDirs) == 0 {
		t.Fatal("no package directories found — this ratchet silently stopped enforcing anything")
	}

	for dir := range pkgDirs {
		fset := token.NewFileSet()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}

		declared := map[string]decl{}            // unexported plain funcs in non-test files
		refsFrom := map[string]map[string]bool{} // caller -> identifiers it mentions ("" = roots)
		exempt := map[string]bool{}

		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			isTest := strings.HasSuffix(e.Name(), "_test.go")
			path := filepath.Join(dir, e.Name())
			f, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if perr != nil {
				continue // generated or otherwise unparseable; not this check's business
			}
			if isTest {
				continue // test files neither declare nor count as production use
			}

			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || fn.Name == nil {
					continue // methods are reachable through interfaces; skip them
				}
				name := fn.Name.Name
				if name == "" || name == "init" || name == "main" {
					continue
				}
				if r := []rune(name)[0]; !unicode.IsLower(r) {
					continue // exported: callable from anywhere, absence proves nothing
				}
				declared[name] = decl{file: path, line: fset.Position(fn.Pos()).Line}
				if fn.Doc != nil {
					for _, c := range fn.Doc.List {
						if strings.Contains(c.Text, deadHelperExemptMarker) {
							exempt[name] = true
						}
					}
				}
			}

			// Build the call graph instead of a reference tally.
			//
			// For every top-level declaration, record which identifiers its body mentions.
			// Anything callable from outside this package — an exported function, ANY method
			// (a method can be reached through an interface), init, main — becomes a root,
			// as does every package-level var or const initialiser, which runs whether or
			// not anything calls it. Everything else is only alive if a root can reach it.
			for _, d := range f.Decls {
				var from string // "" means a root: package-level init or an exported entry
				if fn, ok := d.(*ast.FuncDecl); ok && fn.Name != nil {
					name := fn.Name.Name
					isMethod := fn.Recv != nil
					isExported := len([]rune(name)) > 0 && !unicode.IsLower([]rune(name)[0])
					if isMethod || isExported || name == "init" || name == "main" {
						from = "" // a root; its references seed the walk
					} else {
						from = name
					}
				}
				edges := refsFrom[from]
				if edges == nil {
					edges = map[string]bool{}
					refsFrom[from] = edges
				}
				ast.Inspect(d, func(n ast.Node) bool {
					if id, ok := n.(*ast.Ident); ok {
						edges[id.Name] = true
					}
					return true
				})
			}
		}

		// A declaration always mentions its own name; that self-edge would make every
		// function trivially reachable from itself, so drop it before walking.
		for name, edges := range refsFrom {
			if name != "" {
				delete(edges, name)
			}
		}

		// Walk out from the roots. Exempt functions are roots too: their doc comment asserts
		// they are reached by something this parser cannot see, and whatever they call is
		// legitimately alive as well.
		reachable := map[string]bool{}
		queue := []string{""}
		for name := range exempt {
			queue = append(queue, name)
			reachable[name] = true
		}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for ref := range refsFrom[cur] {
				if reachable[ref] {
					continue
				}
				if _, isLocalFunc := declared[ref]; !isLocalFunc {
					continue // not an unexported function of this package
				}
				reachable[ref] = true
				queue = append(queue, ref)
			}
		}

		for name, where := range declared {
			if exempt[name] || reachable[name] {
				continue
			}
			offenders = append(offenders, where.file+":"+itoaLine(where.line)+": "+name)
		}
	}

	sort.Strings(offenders)
	for _, o := range offenders {
		t.Errorf("%s is unreachable from any production entry point of its package.\n"+
			"  No exported function, method, init, or package-level initialiser can reach it, directly\n"+
			"  or through any chain of unexported calls. It is alive only from tests, or from nothing,\n"+
			"  or only from other code that is itself unreachable.\n"+
			"  Either call it from the path it was written for, or delete it. A helper reachable only\n"+
			"  from its own test passes its tests and does nothing — that is how the provider-stated\n"+
			"  context limit shipped parsed-but-ignored.\n"+
			"  If it is genuinely reached indirectly (registration, reflection), say so in its doc\n"+
			"  comment with %q and the reason.", o, deadHelperExemptMarker)
	}
}

func itoaLine(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
