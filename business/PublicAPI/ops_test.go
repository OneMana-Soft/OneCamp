package business

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// opsForTest gives a test its own registry of ops, and of routes
// (resetForTest). A unit test has no audit log, so a write's record goes
// nowhere (TestAWriteIsRecordedBeforeItIsServedWithWhoMadeIt reads them).
func opsForTest(t *testing.T) {
	t.Helper()
	resetForTest(t)
	saved, savedAudit := ops, audit
	ops = nil
	audit = func(context.Context, *uuid.UUID, string, string, string, string, string, map[string]interface{}) error {
		return nil
	}
	t.Cleanup(func() { ops, audit = saved, savedAudit })
}

// mountOps registers ops as one resource and mounts the API, as router.go does.
func mountOps(t *testing.T, list ...Op) *chi.Mux {
	t.Helper()
	opsForTest(t)
	RegisterOps(Resource{Name: "things", Owner: "test", Scopes: []ScopeDecl{{Name: "things:read"}}}, list)
	r := chi.NewRouter()
	Mount(r, Middleware{RequireScope: requireHeaderScope})
	return r
}

const thingInput = `{"type":"object","properties":{
	"id":{"type":"string"},"name":{"type":"string","maxLength":20},
	"ids":{"type":"array","items":{"type":"string"},"maxItems":3},
	"limit":{"type":"integer"},"cursor":{"type":"string"},"version":{"type":"integer"}},
	"required":["id"]}`

func okServe(context.Context, Call) (any, error) { return map[string]any{"ok": true}, nil }

// thing is an op on one thing, the thing named by the path.
func thing(name string, b Behaviour, serve func(context.Context, Call) (any, error)) Op {
	return Op{Name: name, Summary: "A thing", Method: http.MethodPost, Path: "/things/{id}/" + strings.ReplaceAll(name, ".", "-"),
		Scope: "things:read", Behaviour: b, Input: json.RawMessage(thingInput),
		Reach: func(args map[string]any) (string, string) { s, _ := args["id"].(string); return "thing", s },
		Serve: serve}
}

func TestAnOpIsAWriteExactlyWhenItIsNotReadOnly(t *testing.T) {
	mountOps(t, thing("things.read", Behaviour{ReadOnly: true}, okServe), thing("things.change", Behaviour{}, okServe))
	if n := len(Ops()); n != 2 {
		t.Fatalf("Ops() has %d ops, want 2", n)
	}
	routes := 0
	for _, res := range Resources() {
		for _, rt := range res.Routes {
			routes++
			if want := strings.HasSuffix(rt.Path, "things-change"); rt.Write != want {
				t.Errorf("%s: Write = %v, want %v: an op's Write comes from its Behaviour", rt.Path, rt.Write, want)
			}
		}
	}
	if routes != 2 {
		t.Fatalf("%d routes registered, want one per op", routes)
	}
}

func TestAnOpNeedsExactlyOneWayToHoldAnApp(t *testing.T) {
	opsForTest(t)
	none := thing("things.none", Behaviour{ReadOnly: true}, okServe)
	none.Reach = nil
	two := thing("things.two", Behaviour{ReadOnly: true}, okServe)
	two.AppsInside = true
	for _, op := range []Op{none, two} {
		msg := panics(func() { RegisterOps(Resource{Name: op.Name, Owner: "test"}, []Op{op}) })
		if !strings.Contains(msg, "exactly one of Reach, AppsInside or AppsRefused") {
			t.Errorf("%s: RegisterOps panicked with %q", op.Name, msg)
		}
	}
}

func TestArgumentsAreBuiltOnceAndAPathIdCannotBeContradicted(t *testing.T) {
	var costed, served map[string]any
	op := thing("things.read", Behaviour{ReadOnly: true}, func(_ context.Context, c Call) (any, error) {
		served = c.Args
		return map[string]any{"ok": true}, nil
	})
	op.Cost = func(args map[string]any) int { costed = args; return 1 }
	h := mountOps(t, op)
	w := send(h, http.MethodPost, "/things/t1/things-read", "things:read", `{"name":"a"}`)
	if w.Code != http.StatusOK || served["id"] != "t1" || served["name"] != "a" {
		t.Fatalf("got %d %s; Serve had %v", w.Code, w.Body, served)
	}
	if reflect.ValueOf(costed).Pointer() != reflect.ValueOf(served).Pointer() {
		t.Errorf("Cost and Serve were handed different arguments: %v, %v", costed, served)
	}
	if w := send(h, http.MethodPost, "/things/t1/things-read", "things:read", `{"id":"t1"}`); w.Code != http.StatusOK {
		t.Errorf("a body naming the path's own id: got %d %s", w.Code, w.Body)
	}
	served = nil
	w = send(h, http.MethodPost, "/things/t1/things-read", "things:read", `{"id":"t2"}`)
	if w.Code != http.StatusBadRequest || errorOf(t, w).Code != "conflicting_argument" || served != nil {
		t.Errorf("a body naming another id than the path's: got %d %s, served %v", w.Code, w.Body, served)
	}
}

func TestTheSchemaRefusesWhatItDoesNotDeclare(t *testing.T) {
	h := mountOps(t, thing("things.read", Behaviour{ReadOnly: true}, okServe))
	for _, body := range []string{`{"colour":"red"}`, `{"name":7}`, `{"ids":["a","b","c","d"]}`,
		`{"name":"` + strings.Repeat("x", 21) + `"}`, `{"limit":1.5}`, `[1]`} {
		w := send(h, http.MethodPost, "/things/t1/things-read", "things:read", body)
		if w.Code != http.StatusBadRequest || errorOf(t, w).Code != "invalid_argument" {
			t.Errorf("%s: got %d %s, want 400 invalid_argument", body, w.Code, w.Body)
		}
	}
	loose := thing("things.loose", Behaviour{ReadOnly: true}, okServe)
	loose.Input = json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","pattern":"^t"}}}`)
	opsForTest(t)
	if msg := panics(func() { RegisterOps(Resource{Name: "loose", Owner: "test"}, []Op{loose}) }); !strings.Contains(msg, "pattern") {
		t.Errorf("a keyword the validator doesn't enforce must be refused at registration; panicked with %q", msg)
	}
}

func TestRowsAreChargedOnceValidOnOneCounterAndARefusalSaysWhen(t *testing.T) {
	var charged []string
	refuse := false
	saved := chargeRows
	chargeRows = func(_ context.Context, token string, readOnly bool, n int) error {
		charged = append(charged, fmt.Sprintf("%q %v %d", token, readOnly, n))
		if refuse {
			return &Error{Status: http.StatusTooManyRequests, Code: "row_budget", Message: "spent", RetryAfter: 30}
		}
		return nil
	}
	t.Cleanup(func() { chargeRows = saved })
	served := 0
	op := thing("things.read", Behaviour{ReadOnly: true}, func(context.Context, Call) (any, error) { served++; return map[string]any{}, nil })
	op.Cost = func(args map[string]any) int { ids, _ := args["ids"].([]any); return len(ids) }
	h := mountOps(t, op)
	path := "/things/t1/things-read"
	send(h, http.MethodPost, path, "things:read", `{"ids":["a","b","c","d"]}`)
	send(h, http.MethodPost, path, "things:read", `{"ids":["a","b"]}`)
	send(h, http.MethodPost, path, "things:read", `{}`)
	if got, want := strings.Join(charged, ", "), `"" true 2, "" true 1`; got != want {
		t.Errorf("charged %s, want %s: an invalid call spends nothing, a call spends at least 1", got, want)
	}
	refuse = true
	w := send(h, http.MethodPost, path, "things:read", `{"ids":["a"]}`)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "30" || served != 2 {
		t.Errorf("a spent budget: got %d, Retry-After %q, served %d times; want 429, 30, and Serve not run", w.Code, w.Header().Get("Retry-After"), served)
	}
}

func TestPageArgsAreTheOnePageShape(t *testing.T) {
	for _, c := range []struct {
		args   map[string]any
		cursor string
		limit  int
	}{
		{map[string]any{}, "", 50},
		{map[string]any{"limit": json.Number("7"), "cursor": "abc"}, "abc", 7},
		{map[string]any{"limit": float64(500)}, "", 200},
		{map[string]any{"limit": 0}, "", 1},
	} {
		if cursor, limit := PageArgs(c.args); cursor != c.cursor || limit != c.limit {
			t.Errorf("PageArgs(%v) = %q, %d; want %q, %d", c.args, cursor, limit, c.cursor, c.limit)
		}
	}
}

func TestAVersionedWriteReadsIfMatchAndAnswersAStaleOne412(t *testing.T) {
	op := thing("things.change", Behaviour{}, func(_ context.Context, c Call) (any, error) {
		if fmt.Sprint(c.Args["version"]) != "7" {
			return nil, ErrVersion
		}
		return versioned{Name: "t1", V: 8}, nil
	})
	op.Versioned = true
	h := mountOps(t, op)
	path := "/things/t1/things-change"
	w := send(h, http.MethodPost, path, "things:read", `{}`, "If-Match", `W/"7"`)
	if w.Code != http.StatusCreated || w.Header().Get("ETag") != `W/"8"` {
		t.Errorf("a current version: got %d, ETag %q", w.Code, w.Header().Get("ETag"))
	}
	w = send(h, http.MethodPost, path, "things:read", `{}`, "If-Match", `W/"6"`)
	if w.Code != http.StatusPreconditionFailed || errorOf(t, w).Code != "version_mismatch" {
		t.Errorf("a stale version: got %d %s, want 412 version_mismatch", w.Code, w.Body)
	}
	w = send(h, http.MethodPost, path, "things:read", `{"version":5}`, "If-Match", `W/"7"`)
	if w.Code != http.StatusBadRequest || errorOf(t, w).Code != "conflicting_argument" {
		t.Errorf("a body version against If-Match: got %d %s, want 400 conflicting_argument", w.Code, w.Body)
	}
}

// A cell's value may be any JSON value, null included (its field's kind checks
// it), and a row's values are a map from field to value. So a node with no type
// is any value, left to the op's own checks, and an inner object's
// additionalProperties may be the schema each of its other keys keeps. A
// call's arguments are still declared one by one.
func TestAMapChecksEveryValueAndAnUntypedValueIsLeftToItsOp(t *testing.T) {
	in, err := compileSchema(json.RawMessage(`{"type":"object","properties":{
		"values":{"type":"object","additionalProperties":{}},
		"labels":{"type":"object","properties":{"lead":{"type":"string"}},"additionalProperties":{"type":"string","maxLength":3}},
		"filter":{"description":"any value: the query checks it"}}}`))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, ok := range []string{
		`{"values":{"f1":"x","f2":3,"f3":true,"f4":null,"f5":["a"],"f6":{"k":1}}}`,
		`{"labels":{"lead":"anything long","a":"abc"}}`,
		`{"filter":null}`, `{"filter":{"and":[]}}`, `{"filter":"x"}`,
	} {
		if err := in.check(argsFrom(t, ok)); err != nil {
			t.Errorf("%s: refused with %v", ok, err)
		}
	}
	for args, want := range map[string]string{
		`{"labels":{"a":"abcd"}}`: "labels.a", `{"labels":{"a":1}}`: "labels.a", `{"labels":{"lead":1}}`: "labels.lead",
		`{"other":1}`: "other",
	} {
		if err := in.check(argsFrom(t, args)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want a refusal naming %s", args, err, want)
		}
	}
	for _, bad := range []string{
		`{"type":"object","properties":{"id":{"type":"string"}},"additionalProperties":{"type":"string"}}`,
		`{"type":"object","properties":{"m":{"type":"object","additionalProperties":true}}}`,
		`{"type":"object","properties":{"v":{"properties":{}}}}`,
		`{"type":"object","properties":{"v":{"type":"null"}}}`,
		`{"type":"object","properties":{"v":{"type":["string","null"]}}}`,
	} {
		if _, err := compileSchema(json.RawMessage(bad)); err == nil {
			t.Errorf("%s compiled; want a refusal", bad)
		}
	}
}

// argsFrom is a call's arguments as a body gives them: numbers as written.
func argsFrom(t *testing.T, body string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	return m
}
