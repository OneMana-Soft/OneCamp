package models

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The app numbers these the same way, by position (onecamp-fe
// services/mqttService.ts, pinned by mqttService.enum.test.ts). Inserting a
// type anywhere but the end shifts every one after it, and the app then reads
// each message as something else.
func TestMessageTypesKeepTheirNumbers(t *testing.T) {
	if MESSAGE_AI_AGENT_WORK != 28 || MESSAGE_SAVED_ITEM_DUE != 29 {
		t.Fatalf("AI_AGENT_WORK=%d SAVED_ITEM_DUE=%d, want 28 and 29; append new types at the end", MESSAGE_AI_AGENT_WORK, MESSAGE_SAVED_ITEM_DUE)
	}
	if MESSAGE_POLL_UPDATE != 30 || MESSAGE_SCHEDULED_MESSAGE != 31 {
		t.Fatalf("POLL_UPDATE=%d SCHEDULED_MESSAGE=%d, want 30 and 31", MESSAGE_POLL_UPDATE, MESSAGE_SCHEDULED_MESSAGE)
	}
	if MESSAGE_TASK_DATES != 32 || MESSAGE_TASK_FIELD != 33 {
		t.Fatalf("TASK_DATES=%d TASK_FIELD=%d, want 32 and 33", MESSAGE_TASK_DATES, MESSAGE_TASK_FIELD)
	}
	if MESSAGE_CHAT_SEEN != 34 {
		t.Fatalf("CHAT_SEEN=%d, want 34", MESSAGE_CHAT_SEEN)
	}
	if MESSAGE_CONTENT_IO_JOB != 35 || MESSAGE_DOC_META != 36 || MESSAGE_NOTICE != 37 {
		t.Fatalf("CONTENT_IO_JOB=%d DOC_META=%d NOTICE=%d, want 35, 36 and 37", MESSAGE_CONTENT_IO_JOB, MESSAGE_DOC_META, MESSAGE_NOTICE)
	}
	if MESSAGE_CONVERSATION_ACCESS != 38 {
		t.Fatalf("CONVERSATION_ACCESS=%d, want 38", MESSAGE_CONVERSATION_ACCESS)
	}
	if MESSAGE_DESKTOP_NOTICE != 39 || MESSAGE_TASK_CHANGED != 40 {
		t.Fatalf("DESKTOP_NOTICE=%d TASK_CHANGED=%d, want 39 and 40", MESSAGE_DESKTOP_NOTICE, MESSAGE_TASK_CHANGED)
	}
	if MESSAGE_PIN != 41 {
		t.Fatalf("PIN=%d, want 41", MESSAGE_PIN)
	}
	if MESSAGE_READ_MOVED != 46 {
		t.Fatalf("READ_MOVED=%d, want 46 (the integrator's allotment)", MESSAGE_READ_MOVED)
	}
	// 41 is chat's MESSAGE_PIN (the coordinator's numbering, 17 Oct 2026).
	if MESSAGE_CALL_STATE != 42 {
		t.Fatalf("CALL_STATE=%d, want 42 (41 is chat's MESSAGE_PIN)", MESSAGE_CALL_STATE)
	}
}

// No two message types share a number. The app tells messages apart by their
// number alone (onecamp-fe services/mqttService.ts), so two types on one
// number are read as each other. Numbers come from position in the iota block
// above; an explicit number, or a type declared somewhere else, is how two
// could ever meet, as chat's MESSAGE_PIN and MESSAGE_CALL_STATE both took 41
// on 17 Oct 2026. So this reads the package's source, works out the number of
// every MESSAGE_ constant that is one, and fails on the first number taken twice.
func TestNoTwoMessageTypesShareANumber(t *testing.T) {
	numbers := messageTypeNumbers(t)
	// The reading is checked against the compiler on types near either end.
	for name, want := range map[string]int{"MESSAGE_POST": MESSAGE_POST, "MESSAGE_AI_NUDGE": MESSAGE_AI_NUDGE, "MESSAGE_TASK_CHANGED": MESSAGE_TASK_CHANGED, "MESSAGE_CALL_STATE": MESSAGE_CALL_STATE} {
		if got, ok := numbers[name]; !ok || got != want {
			t.Fatalf("read %s as %d (found %v), the compiler says %d: the reading below is wrong", name, got, ok, want)
		}
	}
	taken := map[int]string{}
	names := make([]string, 0, len(numbers))
	for name := range numbers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		n := numbers[name]
		if other, ok := taken[n]; ok {
			t.Errorf("%s and %s are both %d: the app would read one as the other. Append a new type at the end of the block instead", other, name, n)
		}
		taken[n] = name
	}
}

// messageTypeNumbers reads every MESSAGE_ constant with a whole-number value
// in this package's source (tests aside): iota, a number, or iota plus or
// minus a number. Strings and booleans (MESSAGE_ACTIVITY_*, MESSAGE_CALL_*)
// aren't message types. Anything else fails, which keeps the types plain.
func messageTypeNumbers(t *testing.T) map[string]int {
	t.Helper()
	fset := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			var last []ast.Expr
			for i, spec := range g.Specs {
				vs := spec.(*ast.ValueSpec)
				values := vs.Values
				if len(values) == 0 {
					values = last // a bare name repeats the line above, at its own iota
				} else {
					last = values
				}
				for j, name := range vs.Names {
					if !strings.HasPrefix(name.Name, "MESSAGE_") || j >= len(values) {
						continue
					}
					n, whole, err := wholeNumber(values[j], i)
					if err != nil {
						t.Fatalf("%s: %v", fset.Position(name.Pos()), err)
					}
					if whole {
						out[name.Name] = n
					}
				}
			}
		}
	}
	return out
}

// wholeNumber is the value of a constant's expression at the given iota, and
// whether it is a whole number at all.
func wholeNumber(e ast.Expr, iota int) (int, bool, error) {
	switch v := e.(type) {
	case *ast.ParenExpr:
		return wholeNumber(v.X, iota)
	case *ast.Ident:
		switch v.Name {
		case "iota":
			return iota, true, nil
		case "true", "false":
			return 0, false, nil
		}
	case *ast.BasicLit:
		switch v.Kind {
		case token.INT:
			n, err := strconv.Atoi(v.Value)
			return n, err == nil, err
		case token.STRING, token.CHAR:
			return 0, false, nil
		}
	case *ast.BinaryExpr:
		if v.Op == token.ADD || v.Op == token.SUB {
			a, okA, errA := wholeNumber(v.X, iota)
			b, okB, errB := wholeNumber(v.Y, iota)
			if errA == nil && errB == nil && okA && okB {
				if v.Op == token.SUB {
					return a - b, true, nil
				}
				return a + b, true, nil
			}
		}
	}
	return 0, false, fmt.Errorf("can't read this message type's number; keep message types plain iota")
}

// A project's statuses changed (board columns): 43 outright, past the 41 and
// 42 chat and calls keep; never one an earlier type has.
func TestProjectStatusesKeepsItsNumber(t *testing.T) {
	if MESSAGE_PROJECT_STATUSES != 43 {
		t.Fatalf("PROJECT_STATUSES=%d, want 43", MESSAGE_PROJECT_STATUSES)
	}
	if MESSAGE_TASK_CHANGED >= MESSAGE_PROJECT_STATUSES-2 {
		t.Fatalf("TASK_CHANGED=%d reaches the numbers kept for chat and calls (41, 42) or this one (43)", MESSAGE_TASK_CHANGED)
	}
}
