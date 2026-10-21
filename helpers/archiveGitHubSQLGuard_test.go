package helpers

import (
	"go/ast"
	"go/token"
	"regexp"
	"strings"
	"testing"
)

// sqlLiteral is a string that reads as a statement.
var sqlLiteral = regexp.MustCompile(`(?is)^\s*(SELECT\s.+\sFROM|UPDATE\s+\w+\s+SET|INSERT\s+INTO|DELETE\s+FROM|WITH\s+\w+\s+AS\s*\()`)

// The Archive and GitHub business packages write no SQL and touch no
// connection (backend audit U3: business/Archive ran 34 statements itself,
// on 35 uses of the pool, and business/GitHub passed its own SQL to the
// models). Their SQL is in models/postgres/Archive and GitHubLink. U4 added
// business/Notification: "notify anyway" counted and wrote urgent pings
// itself (models/postgres/UrgentPing).
func TestArchiveAndGitHubBusinessWriteNoSQL(t *testing.T) {
	for _, dir := range []string{"../business/Archive", "../business/GitHub", "../business/Notification"} {
		goFuncs(t, []string{dir}, nil, func(path string, fd *ast.FuncDecl) {
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.BasicLit:
					if x.Kind == token.STRING && sqlLiteral.MatchString(strings.Trim(x.Value, "`\"")) {
						t.Errorf("%s: %s writes SQL; put it in the package's models", path, fd.Name.Name)
					}
				case *ast.SelectorExpr:
					if x.Sel.Name == "DBConn" {
						t.Errorf("%s: %s touches the database connection; go through the models", path, fd.Name.Name)
					}
				}
				return true
			})
		})
	}
}
