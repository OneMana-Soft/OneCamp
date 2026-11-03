package controllers

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// aiWritesNotRecorded are the handlers here named for a write that change no
// workspace AI setting, so they write no AI audit row: each is a person's own
// setting or content, or a probe that saves nothing or writes its own record.
var aiWritesNotRecorded = map[string]string{
	"TestConnection":            "a probe: tries the address and key in the request, and saves nothing",
	"RunGovernanceDrill":        "a probe: the drill writes its own audit rows (the attempt and the refusal) and changes no AI setting",
	"RunMyGovernanceDrill":      "a probe: the same drill, run as the member asking, with its own audit rows",
	"SetDailyNotePreference":    "a person's own daily note preference",
	"DeleteChatSession":         "a person's own chat session",
	"SetMyModel":                "a person's own model choice, among the models an admin authorized",
	"SetMyAIInstructions":       "a person's own AI instructions",
	"CreateDocActionItems":      "content: tasks made from a doc's action items",
	"UpdateMemoryStatus":        "content: a memory item's status",
	"UpdateMemoryDue":           "content: a memory item's due date",
	"DeleteMemory":              "content: a memory item",
	"CreateTaskFromMemory":      "content: a task made from a memory item",
	"CreateTasksFromExtraction": "content: tasks made from an extraction",
}

var aiWriteVerb = regexp.MustCompile(`^(Set|Create|Update|Delete|Authorize|Revoke|Pull|Add|Remove|Run|Test|Rebuild|Send)[A-Z]`)

// TestEveryAIChangeIsRecorded holds V-269: every handler here that changes the
// workspace's AI (named for a write: Set, Create, Update, Delete, Authorize,
// Revoke, Pull, Add, Remove, Run, Test, Rebuild, Send) calls recordAIChange,
// unless aiWritesNotRecorded says why not; every handler that calls it is named
// for a write, so deleting a call can never go unseen; and no "AI audit:" log
// line is written anywhere but there.
func TestEveryAIChangeIsRecorded(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var missing, unguarded, logOnly, seen []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && strings.Contains(lit.Value, "AI audit:") && name != "aiAudit.go" {
				logOnly = append(logOnly, fmt.Sprintf("%s:%d", name, fset.Position(lit.Pos()).Line))
			}
			return true
		})
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil || len(fn.Type.Params.List) != 2 {
				continue
			}
			records := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "recordAIChange" {
						records = true
					}
				}
				return !records
			})
			if !aiWriteVerb.MatchString(fn.Name.Name) {
				if records {
					unguarded = append(unguarded, name+" "+fn.Name.Name)
				}
				continue
			}
			if _, exempt := aiWritesNotRecorded[fn.Name.Name]; exempt {
				seen = append(seen, fn.Name.Name)
				if records {
					t.Errorf("%s records an AI change: take it off aiWritesNotRecorded", fn.Name.Name)
				}
				continue
			}
			if !records {
				missing = append(missing, name+" "+fn.Name.Name)
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d AI write handlers record nothing in the admin audit log: call recordAIChange (or list one in aiWritesNotRecorded with why):\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
	sort.Strings(unguarded)
	if len(unguarded) > 0 {
		t.Errorf("%d handlers record an AI change under a name outside aiWriteVerb, so losing the call would go unseen: add the verb:\n  %s",
			len(unguarded), strings.Join(unguarded, "\n  "))
	}
	if len(logOnly) > 0 {
		t.Errorf("%d \"AI audit:\" log lines outside recordAIChange, which a log line can't stand for:\n  %s", len(logOnly), strings.Join(logOnly, "\n  "))
	}
	if len(seen) != len(aiWritesNotRecorded) {
		t.Errorf("aiWritesNotRecorded names %d handlers, %d of them here: drop the stale ones", len(aiWritesNotRecorded), len(seen))
	}
}
