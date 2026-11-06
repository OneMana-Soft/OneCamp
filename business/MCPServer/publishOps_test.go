package business

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

func tableOp(name string, b v1api.Behaviour, input string) v1api.Op {
	return v1api.Op{Name: name, Summary: "A test op", Scope: "tables:write", Behaviour: b, Input: json.RawMessage(input),
		Reach: func(args map[string]any) (string, string) { id, _ := args["id"].(string); return "table", id }}
}

// Every op of the public API is a governed tool declared by the op alone: its
// scope, its behaviour (so its hints), its summary and its Input schema, and
// the table its Reach names, checked by the rows' rule.
func TestEveryAPIOpIsAToolWithItsScopeHintsAndSchema(t *testing.T) {
	ops := dataTableBusiness.APIOps()
	if len(ops) == 0 {
		t.Fatal("the tables ops are gone")
	}
	for _, op := range ops {
		spec, err := specForOp(op)
		if err != nil {
			t.Fatalf("%s: %v", op.Name, err)
		}
		b := op.Behaviour
		if spec.Name != strings.ReplaceAll(op.Name, ".", "_") || spec.Description != op.Summary || spec.RequiredScope != op.Scope ||
			string(spec.InputSchema) != string(op.Input) || spec.Behaviour != (ToolBehaviour{ReadOnly: b.ReadOnly, Destructive: b.Destructive, Idempotent: b.Idempotent, Recoverable: b.Recoverable}) {
			t.Errorf("%s is published as %+v: not as declared", op.Name, spec)
		}
		id := uuid.NewString()
		if op.Unplaced != "" {
			// An op on no place acts on its kind alone, whatever it is sent:
			// never on a table an argument names.
			want := opKinds[op.Unplaced]
			for _, args := range []map[string]any{{"id": id}, {}} {
				if ref, err := spec.Resource(args); err != nil || ref.Kind != want || ref.ID != "" || !want.IdentifiesNoObject() {
					t.Errorf("%s acts on %+v (%v) given %v, want a %s and no object", op.Name, ref, err, args, want)
				}
			}
		} else {
			if ref, err := spec.Resource(map[string]any{"id": id}); err != nil || ref.Kind != ResourceTableRows || ref.ID != id {
				t.Errorf("%s acts on %+v (%v), want the rows of table %s", op.Name, ref, err, id)
			}
			if _, err := spec.Resource(map[string]any{}); err == nil {
				t.Errorf("%s with no table named acts on something", op.Name)
			}
		}
		// A key exactly for a write that isn't idempotent (a batch that makes
		// rows): a read, or the same cells set twice, needs none.
		if wants := !b.ReadOnly && !b.Idempotent; (spec.IdempotencyKey != nil) != wants {
			t.Errorf("%s asks for an idempotency key: %v; want %v (read-only %v, idempotent %v)", op.Name, spec.IdempotencyKey != nil, wants, b.ReadOnly, b.Idempotent)
		}
	}
}

// An op that can't be published stops startup: a name a registry tool has, a
// place with no authority rule, or none named.
func TestAnOpThatCantBePublishedStopsStartup(t *testing.T) {
	rows := `{"type":"object","properties":{"id":{"type":"string"}}}`
	planet := tableOp("planets.list", v1api.Behaviour{ReadOnly: true}, rows)
	planet.Reach = func(map[string]any) (string, string) { return "planet", "" }
	blind := tableOp("tables.blind", v1api.Behaviour{ReadOnly: true}, rows)
	blind.Reach = nil
	both := tableOp("tables.both", v1api.Behaviour{}, rows)
	both.Unplaced = "self"
	nowhere := tableOp("tables.nowhere", v1api.Behaviour{}, rows)
	nowhere.Reach, nowhere.Unplaced = nil, "planet"
	for name, op := range map[string]v1api.Op{
		"read.table, read_table as a tool": tableOp("read.table", v1api.Behaviour{ReadOnly: true}, rows),
		"a planet":                         planet,
		"no Reach":                         blind,
		"a place and none":                 both,
		"no place, of a kind with no rule": nowhere,
	} {
		if err := publish([]v1api.Op{op}); err == nil {
			t.Errorf("%s: published", name)
		}
		if _, ok := Lookup(ToolNameForOp(op.Name)); ok && name != "read.table, read_table as a tool" {
			t.Errorf("%s: registered all the same", name)
		}
	}
}

// A write that isn't safe to repeat gets the bridged tools' key, from its
// declared arguments alone.
func TestANonIdempotentOpIsKeyedByItsArguments(t *testing.T) {
	spec, err := specForOp(tableOp("tables.rows.create", v1api.Behaviour{},
		`{"type":"object","properties":{"id":{"type":"string"},"values":{"type":"object"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if spec.IdempotencyKey == nil {
		t.Fatal("a write not safe to repeat has no idempotency key")
	}
	key := func(args map[string]any) string {
		k, err := spec.IdempotencyKey(args)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	a := map[string]any{"id": "t1", "values": map[string]any{"f": "x"}}
	if key(a) != key(map[string]any{"id": "t1", "values": map[string]any{"f": "x"}, "note": "undeclared"}) {
		t.Error("an argument the op doesn't declare changed the call's identity")
	}
	if key(a) == key(map[string]any{"id": "t1", "values": map[string]any{"f": "y"}}) {
		t.Error("two different writes share a key")
	}
}

// A published op runs only as the principal the ladder authorised, and a
// refusal in the API's words reaches the caller as a ToolRefusal.
func TestAPublishedOpRunsOnlyAsItsPrincipal(t *testing.T) {
	member := userModels.UserInfo{}
	member.UserPostgresInfo.Id = uuid.New()
	token := uuid.NewString()
	ctx := context.WithValue(context.Background(), helpers.UserInfoContextKey, member)
	ctx = context.WithValue(ctx, helpers.ApiTokenIDContextKey, token)
	in := ToolCallContext{PrincipalUserID: member.UserPostgresInfo.Id.String(), TokenID: token}

	var refusal *ToolRefusal
	for name, c := range map[string]ToolCallContext{
		"someone else":  {PrincipalUserID: uuid.NewString(), TokenID: token},
		"another token": {PrincipalUserID: in.PrincipalUserID, TokenID: uuid.NewString()},
		"nobody at all": {},
	} {
		if _, err := serveOp(ctx, "tables.nothing", c); err == nil || errors.As(err, &refusal) {
			t.Errorf("%s: ran, or was refused in the caller's words (%v)", name, err)
		}
	}
	_, err := serveOp(ctx, "tables.nothing", in)
	if !errors.As(err, &refusal) || refusal.Code != "not_found" || refusal.Message == "" {
		t.Errorf("an op the API hasn't got answered %v, want its not_found in a ToolRefusal", err)
	}
}

func TestAResultIsItsTextOrItsJSON(t *testing.T) {
	type row struct {
		ID string `json:"id"`
	}
	long := strings.Repeat("é", maxToolOutputRunes+10)
	for _, c := range []struct {
		out  any
		want string
	}{
		{map[string]any{"text": "hi", "meta": 1}, "hi"},
		{map[string]any{"meta": 1}, ""},
		{nil, ""},
		{row{ID: "r1"}, `{"id":"r1"}`},
	} {
		if got := ResultText(c.out); got != c.want {
			t.Errorf("ResultText(%v) = %q, want %q", c.out, got, c.want)
		}
	}
	if got := []rune(ResultText([]string{long})); len(got) > maxToolOutputRunes+3 {
		t.Errorf("a long result's text is %d runes, past the cut", len(got))
	}
}

// The tables ops that act on no table are checked as the bridged tools of
// their kind are (H2c): a table made is the caller's own, in no container
// (ResourceSelfOwned, a write with the creations' key), and the deleted
// tables are a read the handler holds to the caller (ResourceWorkspace, which
// refuses every write).
func TestTheTablesOpsOnNoTableAreTheirKindsTools(t *testing.T) {
	want := map[string]ResourceKind{"tables.table.create": ResourceSelfOwned, "tables.deleted.list": ResourceWorkspace}
	for _, op := range dataTableBusiness.APIOps() {
		kind, ok := want[op.Name]
		if !ok {
			if op.Unplaced != "" {
				t.Errorf("%s acts on no place (%s); only %v do", op.Name, op.Unplaced, want)
			}
			continue
		}
		delete(want, op.Name)
		spec, err := specForOp(op)
		if err != nil {
			t.Fatalf("%s: %v", op.Name, err)
		}
		ref, err := spec.Resource(map[string]any{"id": uuid.NewString()})
		if err != nil || ref.Kind != kind || ref.ID != "" {
			t.Errorf("%s acts on %+v (%v), want a %s", op.Name, ref, err, kind)
		}
		if kind == ResourceSelfOwned && (op.Behaviour.ReadOnly || op.Behaviour.Idempotent || spec.IdempotencyKey == nil) {
			t.Errorf("%s makes a table: a write not safe to repeat, with the creations' key; it is %+v", op.Name, op.Behaviour)
		}
		if kind == ResourceWorkspace && !op.Behaviour.ReadOnly {
			t.Errorf("%s writes on the workspace's say alone", op.Name)
		}
	}
	if len(want) != 0 {
		t.Errorf("the tables ops leave out %v", want)
	}
}
