package helpers

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A compensating delete must not be assigned into err.
//
// WHY THIS EXISTS, five times over. The pattern below was hand-written at five separate call
// sites and got the same thing wrong at every one of them:
//
//	_, err = domain.CreateOrUpdateDgraphPost(ctx, &dgraphPost)
//	if err != nil {
//	        log(err)
//	        err = domain.HardDeletePostByUUIUD(ctx, postUUID) // <-- clobbers the real error
//	        if err != nil {
//	                log(err)
//	        }
//	        return                                            // <-- returns nil on success
//	}
//
// The rollback's result overwrites the failure that caused it. When the rollback SUCCEEDS — the
// ordinary case — err becomes nil and the naked return reports success. In CreatePost and both
// chat paths that meant the controller answered 200 "created post successfully!" with a null
// payload for a message it had just deleted: the sender saw it delivered, and it disappeared on
// refresh. CreateTeam and CreateProject had the same defect.
//
// Every one of those five was written by someone who had seen the other four. That is the
// argument for a check rather than another round of review: the shape reads correctly, because
// assigning to err is what you do everywhere else in the function.
//
// helpers.CompensateOnFailure is the fix and it cannot make this mistake — it takes the undo as
// a closure and returns its own error, leaving the caller's err alone. It also runs the undo on
// a context.WithoutCancel, which matters because a cancelled request is a plausible cause of the
// original write failing, and the old code's rollback would then fail too and leave the orphan
// it was written to prevent.
func TestCompensatingDeleteIsNotAssignedIntoErr(t *testing.T) {
	// Every layer that performs writes. Listed explicitly, and each is required to exist, so a
	// move or rename cannot turn this into a check that scans nothing and passes.
	roots := []string{
		"../business",
		"../controllers",
		"../domain",
		"../models",
		"../services",
	}

	var offenders []string
	for _, root := range roots {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("scan root %s missing — this check would silently pass: %v", root, err)
		}
		walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}

			fset := token.NewFileSet()
			file, parseErr := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if parseErr != nil {
				return parseErr
			}

			ast.Inspect(file, func(n ast.Node) bool {
				ifStmt, ok := n.(*ast.IfStmt)
				if !ok || ifStmt.Body == nil || !conditionTestsErr(ifStmt.Cond) {
					return true
				}
				// Only the statements directly inside this error branch. Nested ifs are
				// visited on their own by Inspect.
				for _, stmt := range ifStmt.Body.List {
					assign, ok := stmt.(*ast.AssignStmt)
					if !ok || assign.Tok != token.ASSIGN {
						continue
					}
					if !assignsToErr(assign.Lhs) {
						continue
					}
					name, isCall := calledFunctionName(assign.Rhs)
					if !isCall || !looksLikeCompensation(name) {
						continue
					}
					offenders = append(offenders, fmt.Sprintf(
						"%s:%d: err = %s(...) inside an error branch — the rollback's result "+
							"replaces the failure that caused it; use helpers.CompensateOnFailure",
						path, fset.Position(assign.Pos()).Line, name))
				}
				return true
			})
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", root, walkErr)
		}
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("compensating deletes assigned into err (%d):\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// conditionTestsErr reports whether an if-condition is checking an error, i.e. `err != nil`,
// `err == nil`, or a helper such as `helpers.DgraphWriteFailed(uid, err)`.
func conditionTestsErr(cond ast.Expr) bool {
	found := false
	ast.Inspect(cond, func(n ast.Node) bool {
		if ident, ok := n.(*ast.Ident); ok && isErrName(ident.Name) {
			found = true
		}
		return !found
	})
	return found
}

// assignsToErr reports whether the left-hand side of an assignment writes to an error variable.
func assignsToErr(lhs []ast.Expr) bool {
	for _, expr := range lhs {
		if ident, ok := expr.(*ast.Ident); ok && isErrName(ident.Name) {
			return true
		}
	}
	return false
}

func isErrName(name string) bool {
	return name == "err" || strings.HasSuffix(name, "Err")
}

// calledFunctionName returns the called function's own name for a single-call right-hand side.
func calledFunctionName(rhs []ast.Expr) (string, bool) {
	if len(rhs) != 1 {
		return "", false
	}
	call, ok := rhs[0].(*ast.CallExpr)
	if !ok {
		return "", false
	}
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		return fun.Sel.Name, true
	case *ast.Ident:
		return fun.Name, true
	}
	return "", false
}

// looksLikeCompensation reports whether a function name is an undo: the family of calls that
// reverse a write. Restore counts too — channel and project compensation puts a soft-deleted row
// back, and clobbering err there hides the same failure.
func looksLikeCompensation(name string) bool {
	return strings.Contains(name, "Delete") ||
		strings.HasPrefix(name, "Remove") ||
		strings.HasPrefix(name, "Restore")
}
