package notification

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// eventKinds is every EventType constant this package declares, read from its
// source, so a kind added later is held by the tests that range over it
// without anyone remembering to list it.
func eventKinds(t *testing.T) []EventType {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var kinds []EventType
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "EventType" {
					continue
				}
				for _, v := range vs.Values {
					lit, ok := v.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s: an EventType constant that isn't a string literal: %T", name, v)
					}
					s, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					kinds = append(kinds, EventType(s))
				}
			}
		}
	}
	if len(kinds) < 20 {
		t.Fatalf("read only %d EventType constants: the reader has lost them", len(kinds))
	}
	return kinds
}

// The public demo mails no one, whatever happens there (V-1241): every kind,
// and a reminder someone asked to have emailed, stop at Dispatch's first line,
// before any store is asked (the demo makes no preference rows either). The
// first thing Dispatch asks of the world is its Event.MayRead gate, so a gate
// reached is a Dispatch that went on.
func TestTheDemoMailsNoOneForAnyKind(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "re_test")
	t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
	t.Setenv("DEMO_MODE", "true")
	ctx := context.Background()
	events := []Event{}
	for _, k := range eventKinds(t) {
		events = append(events, Event{Type: k, Title: "about " + string(k)})
	}
	events = append(events, Event{Type: EventCalendarReminder, Title: "Stand-up in 10 minutes", Requested: true, SkipOnlineCheck: true})
	for _, evt := range events {
		reached := false
		evt.ActorUUID = uuid.NewString()
		evt.Recipients = []Recipient{{UserUUID: uuid.New()}}
		evt.MayRead = func(context.Context, []string) (map[string]bool, error) {
			reached = true
			return nil, context.Canceled
		}
		if n := Dispatch(ctx, evt); n != 0 || reached {
			t.Errorf("%s (requested %v) on the demo: queued %d, went past the first line %v; want 0, false", evt.Type, evt.Requested, n, reached)
		}
	}
}
