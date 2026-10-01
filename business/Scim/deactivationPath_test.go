package business

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Deactivation must keep going through userBusiness.DeactivateUser.
//
// WHY THIS IS A TEST AND NOT A COMMENT. The bypass is the SHORTER implementation, which is what makes it
// likely rather than merely possible. SCIM already reads users directly — models/postgres/Scim exists
// because every lookup in models/postgres/User filters out deactivated rows — so a future change adding
// "just one UPDATE" for deleted_at fits the surrounding code perfectly, passes review, and works.
//
// It also breaks offboarding, silently. DeactivateUser writes the timestamp to Postgres AND to the Dgraph
// node. business/Principal.Assess — the gate stopping a departed employee's api_token from continuing to
// authorize AI agent work — reads the DGRAPH copy. A Postgres-only write therefore produces exactly the
// state that is hardest to notice: the user list says the person is gone, the SCIM API says the person is
// gone, the directory's access review says the person is gone, and every agent they authorized keeps
// running with their permissions.
//
// Nothing else in the system reports that. So the property is asserted against source, here, where the
// mistake would be made.
//
// BOTH CHECKS BELOW IGNORE COMMENTS, and that is not a detail. The first version of the second check was
// a substring search for "userBusiness.DeactivateUser", and it PASSED against a deliberate mutation that
// removed the call — because this file's own prose names the function it is describing. A guard satisfied
// by a comment about the thing it guards is worse than no guard: it reports success for the exact change
// it exists to catch. So one check parses the AST and looks at call expressions, and the other strips
// comments before matching.

// deletedAtWrite matches an UPDATE that assigns users.deleted_at, in any spacing.
var deletedAtWrite = regexp.MustCompile(`(?i)set\s+[^;]*deleted_at\s*=`)

// deletionDomainCall matches the domain functions that write deleted_at. Calling either from this package
// would bypass the Dgraph half just as surely as raw SQL, and reads as more legitimate because it goes
// through a layer.
var deletionDomainCall = regexp.MustCompile(`domain\.UpdateDeletedTime\w*`)

// blockComment and lineComment strip prose so a rule cannot be satisfied by a sentence describing it.
var (
	blockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	lineComment  = regexp.MustCompile(`(?m)^\s*//.*$`)
)

func stripComments(src string) string {
	return lineComment.ReplaceAllString(blockComment.ReplaceAllString(src, ""), "")
}

func TestScimNeverWritesDeletedAtDirectly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("globbing this package: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no source files found; this check would pass vacuously")
	}

	checked := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("reading %s: %v", path, readErr)
		}
		checked++
		code := stripComments(string(raw))

		if deletedAtWrite.MatchString(code) {
			t.Errorf("%s writes users.deleted_at directly. Deactivation must call "+
				"userBusiness.DeactivateUser, which also writes the Dgraph node that "+
				"business/Principal.Assess reads — a Postgres-only write leaves every AI agent the "+
				"person authorized fully operational after they are offboarded.", path)
		}
		if m := deletionDomainCall.FindString(code); m != "" {
			t.Errorf("%s calls %s. That writes only Postgres; use userBusiness.DeactivateUser or "+
				"ActivateUser so the Dgraph copy stays in agreement with it.", path, m)
		}
	}
	if checked == 0 {
		t.Fatal("every file was skipped; this check is inspecting nothing")
	}
}

// The companion property: the deactivation path is actually reached. A guard against writing deleted_at
// directly is worth nothing if the package stopped deactivating anybody at all.
//
// Asserted over CALL EXPRESSIONS from the parsed AST, so neither a comment nor a string literal can
// satisfy it. See the note at the top of this file for the mutation that proved a substring search cannot.
func TestScimCallsTheSharedDeactivationPath(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "scimUsers.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing scimUsers.go: %v", err)
	}

	called := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		called[pkg.Name+"."+sel.Sel.Name] = true
		return true
	})

	if len(called) == 0 {
		t.Fatal("found no qualified calls at all; this check is inspecting nothing")
	}

	for _, want := range []string{
		"userBusiness.DeactivateUser",
		"userBusiness.ActivateUser",
		// The push-token cleanup lives in the admin HTTP handler rather than in DeactivateUser, so calling
		// only the business function leaves a deprovisioned person still receiving mobile notifications.
		"fcmBusiness.DeleteByUserId",
	} {
		if !called[want] {
			t.Errorf("scimUsers.go no longer CALLS %s. If deactivation was reworked, update this test — "+
				"otherwise the guard above is protecting a path nothing uses.", want)
		}
	}
}
