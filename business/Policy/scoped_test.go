package business

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// scopedStore holds values set for scopes, by "kind:id", for the seams.
type scopedStore map[string]map[string]string

// withScopes is withStore with scoped values too, and a test policy that
// projects may set: "Who can change tasks", a choice.
func withScopes(t *testing.T, s *store, sc scopedStore) *Registry {
	t.Helper()
	r := withStore(t, s)
	oldR, oldW, oldC, oldSpecs := readScoped, writeScoped, clearScoped, specs
	t.Cleanup(func() {
		readScoped, writeScoped, clearScoped, specs = oldR, oldW, oldC, oldSpecs
		scopedCache.Purge()
	})
	scopedCache.Purge()
	readScoped = func(_ context.Context, sp Scope) (map[string]string, error) {
		out := map[string]string{}
		for k, v := range sc[sp.cacheKey()] {
			out[k] = v
		}
		return out, nil
	}
	writeScoped = func(_ context.Context, sp Scope, key, value string, _ uuid.UUID) error {
		if sc[sp.cacheKey()] == nil {
			sc[sp.cacheKey()] = map[string]string{}
		}
		sc[sp.cacheKey()][key] = value
		return nil
	}
	clearScoped = func(_ context.Context, sp Scope, key string) error {
		delete(sc[sp.cacheKey()], key)
		return nil
	}
	specs = append(append([]spec(nil), oldSpecs...),
		spec{key: whoEdits.Key, kind: KindChoice, def: whoEdits.Default, choices: whoEdits.Choices, scopes: []string{ScopeProject},
			label: "Who can change tasks"},
		spec{key: limitHard.Key, kind: KindBool, def: limitHard.Default, scopes: []string{ScopeProject}, label: "Hard limits"},
		spec{key: closeAfter.Key, kind: KindInt, def: closeAfter.Default, min: 0, max: 365, scopes: []string{ScopeProject}, label: "Close after"},
	)
	return r
}

var (
	whoEdits   = ChoicePolicy{Key: "test.edit.who", Default: "admins", Choices: []string{"members", "assigned", "admins"}}
	limitHard  = BoolPolicy{Key: "test.limit.hard", Default: false}
	closeAfter = IntPolicy{Key: "test.close.after", Default: 0}
)

// A choice takes one of its values and nothing else, is read back at once,
// and its record says what it was and is.
func TestAChoiceTakesOneOfItsValues(t *testing.T) {
	s := &store{values: map[string]string{}}
	r := withScopes(t, s, scopedStore{})
	ctx := context.Background()
	if got := r.Choice(ctx, whoEdits); got != "admins" {
		t.Fatalf("unset: %q, want the default", got)
	}
	in, err := r.Set(ctx, whoEdits.Key, json.RawMessage(`"members"`), admin)
	if err != nil || in.Value != "members" || !in.IsSet || strings.Join(in.Choices, ",") != "members,assigned,admins" {
		t.Fatalf("set: %+v %v", in, err)
	}
	if r.Choice(ctx, whoEdits) != "members" {
		t.Error("the change wasn't read back")
	}
	if len(s.records) != 1 || s.records[0] != "human|policy.changed|settings|Changed “Who can change tasks” from admins to members" {
		t.Errorf("recorded %v", s.records)
	}
	for _, bad := range []string{`"everyone"`, `true`, `["members"]`, `""`} {
		if _, err := r.Set(ctx, whoEdits.Key, json.RawMessage(bad), admin); !errors.Is(err, ErrBadValue) {
			t.Errorf("%s: %v", bad, err)
		}
	}
	// A stored value that isn't a choice any more reads as the default.
	s.values[whoEdits.Key] = "everyone"
	r.forget()
	if got := r.Choice(ctx, whoEdits); got != "admins" {
		t.Errorf("a stale value reads %q", got)
	}
}

// A project's own value beats the workspace's for that project only; with
// none, the workspace's applies, then the default; clearing it goes back to
// the workspace's. Each change is recorded with its scope.
func TestAScopesOwnValueBeatsTheWorkspaces(t *testing.T) {
	s := &store{values: map[string]string{}}
	sc := scopedStore{}
	r := withScopes(t, s, sc)
	ctx := context.Background()
	launch := Scope{Kind: ScopeProject, ID: uuid.NewString()}
	other := Scope{Kind: ScopeProject, ID: uuid.NewString()}

	if r.ChoiceIn(ctx, whoEdits, launch) != "admins" || r.BoolIn(ctx, limitHard, launch) || r.IntIn(ctx, closeAfter, launch) != 0 {
		t.Fatal("nothing set: the defaults")
	}
	if _, err := r.Set(ctx, whoEdits.Key, json.RawMessage(`"members"`), admin); err != nil {
		t.Fatal(err)
	}
	if r.ChoiceIn(ctx, whoEdits, launch) != "members" {
		t.Error("with no value of its own, a project follows the workspace")
	}
	in, err := r.SetIn(ctx, whoEdits.Key, launch, json.RawMessage(`"assigned"`), admin)
	if err != nil || in.Value != "assigned" || !in.IsSet || in.Default != "members" {
		t.Fatalf("set for the project: %+v %v", in, err)
	}
	if r.ChoiceIn(ctx, whoEdits, launch) != "assigned" || r.ChoiceIn(ctx, whoEdits, other) != "members" || r.Choice(ctx, whoEdits) != "members" {
		t.Error("a project's own value reaches only that project")
	}
	if _, err := r.SetIn(ctx, limitHard.Key, launch, json.RawMessage(`true`), admin); err != nil || !r.BoolIn(ctx, limitHard, launch) || r.BoolIn(ctx, limitHard, other) {
		t.Errorf("a bool for the project: %v", err)
	}
	if _, err := r.SetIn(ctx, closeAfter.Key, launch, json.RawMessage(`30`), admin); err != nil || r.IntIn(ctx, closeAfter, launch) != 30 {
		t.Errorf("an int for the project: %v", err)
	}
	if _, err := r.SetIn(ctx, closeAfter.Key, launch, json.RawMessage(`400`), admin); !errors.Is(err, ErrBadValue) {
		t.Errorf("out of range: %v", err)
	}
	in, err = r.ClearIn(ctx, whoEdits.Key, launch, admin)
	if err != nil || in.Value != "members" || in.IsSet {
		t.Fatalf("cleared: %+v %v", in, err)
	}
	if r.ChoiceIn(ctx, whoEdits, launch) != "members" {
		t.Error("cleared: the workspace's applies again")
	}
	want := []string{
		"human|policy.changed|settings|Changed “Who can change tasks” from admins to members",
		"human|policy.changed|settings|Changed “Who can change tasks” for a project from members to assigned",
		"human|policy.changed|settings|Changed “Hard limits” for a project from off to on",
		"human|policy.changed|settings|Changed “Close after” for a project from 0 to 30",
		"human|policy.changed|settings|Changed “Who can change tasks” for a project from assigned to the workspace's (members)",
	}
	if strings.Join(s.records, "\n") != strings.Join(want, "\n") {
		t.Errorf("recorded:\n%s", strings.Join(s.records, "\n"))
	}
}

// Only a policy whose spec names the scope's kind takes a value for one, and
// only for a real id.
func TestOnlyAScopedPolicyTakesAScope(t *testing.T) {
	r := withScopes(t, &store{values: map[string]string{}}, scopedStore{})
	ctx := context.Background()
	project := Scope{Kind: ScopeProject, ID: uuid.NewString()}
	if _, err := r.SetIn(ctx, DocsExportMembers.Key, project, json.RawMessage(`false`), admin); !errors.Is(err, ErrNotScoped) {
		t.Errorf("a workspace-only policy: %v", err)
	}
	if _, err := r.SetIn(ctx, "test.nothing", project, json.RawMessage(`true`), admin); !errors.Is(err, ErrUnknownPolicy) {
		t.Errorf("no such policy: %v", err)
	}
	if _, err := r.SetIn(ctx, whoEdits.Key, Scope{Kind: "team", ID: uuid.NewString()}, json.RawMessage(`"members"`), admin); !errors.Is(err, ErrNotScoped) {
		t.Errorf("a kind it can't be set for: %v", err)
	}
	if _, err := r.SetIn(ctx, whoEdits.Key, Scope{Kind: ScopeProject, ID: "launch"}, json.RawMessage(`"members"`), admin); !errors.Is(err, ErrNotScoped) {
		t.Errorf("not an id: %v", err)
	}
}

// A scoped change the audit log can't record is undone and refused, as a
// workspace one is.
func TestAnUnrecordedScopedChangeIsUndone(t *testing.T) {
	s := &store{values: map[string]string{}}
	sc := scopedStore{}
	r := withScopes(t, s, sc)
	ctx := context.Background()
	project := Scope{Kind: ScopeProject, ID: uuid.NewString()}
	if _, err := r.SetIn(ctx, whoEdits.Key, project, json.RawMessage(`"members"`), admin); err != nil {
		t.Fatal(err)
	}
	s.failRec = true
	if _, err := r.SetIn(ctx, whoEdits.Key, project, json.RawMessage(`"assigned"`), admin); !errors.Is(err, ErrNotRecorded) {
		t.Fatalf("err %v", err)
	}
	if r.ChoiceIn(ctx, whoEdits, project) != "members" {
		t.Error("the unrecorded change stood")
	}
	if _, err := r.ClearIn(ctx, whoEdits.Key, project, admin); !errors.Is(err, ErrNotRecorded) || r.ChoiceIn(ctx, whoEdits, project) != "members" {
		t.Errorf("an unrecorded clearing stood: %v", err)
	}
	fresh := Scope{Kind: ScopeProject, ID: uuid.NewString()}
	if _, err := r.SetIn(ctx, whoEdits.Key, fresh, json.RawMessage(`"members"`), admin); !errors.Is(err, ErrNotRecorded) {
		t.Fatal(err)
	}
	if _, set := sc[fresh.cacheKey()][whoEdits.Key]; set {
		t.Error("never set before: undoing leaves nothing set")
	}
}

// PR1 (#2): a page of tasks across many projects reads every project's own
// values in one statement (Prime), never one per project; what was primed is
// served from the cache, a scope with no values included, and a failed read
// leaves each scope to its own read.
func TestPrimingReadsManyScopesAtOnce(t *testing.T) {
	sc := scopedStore{}
	r := withScopes(t, &store{values: map[string]string{}}, sc)
	a, b, c := uuid.NewString(), uuid.NewString(), uuid.NewString()
	sc[Scope{Kind: ScopeProject, ID: a}.cacheKey()] = map[string]string{whoEdits.Key: "members"}
	oldMany := readScopedMany
	t.Cleanup(func() { readScopedMany = oldMany })
	var batches [][]string
	readScopedMany = func(_ context.Context, kind string, ids []string) (map[string]map[string]string, error) {
		batches = append(batches, ids)
		out := map[string]map[string]string{}
		for _, id := range ids {
			if v, ok := sc[kind+":"+strings.ToLower(id)]; ok {
				out[strings.ToLower(id)] = v
			}
		}
		return out, nil
	}
	single := 0
	oldR := readScoped
	readScoped = func(ctx context.Context, sp Scope) (map[string]string, error) {
		single++
		return oldR(ctx, sp)
	}
	r.PrimeScopes(context.Background(), ScopeProject, []string{a, b, c, a})
	if len(batches) != 1 || len(batches[0]) != 3 {
		t.Fatalf("one read of the three scopes: %v", batches)
	}
	if got := r.ChoiceIn(context.Background(), whoEdits, Scope{Kind: ScopeProject, ID: a}); got != "members" {
		t.Fatalf("a's own value: %s", got)
	}
	if got := r.ChoiceIn(context.Background(), whoEdits, Scope{Kind: ScopeProject, ID: b}); got != "admins" {
		t.Fatalf("b has none: the workspace's: %s", got)
	}
	if single != 0 {
		t.Fatalf("a primed scope was read again on its own %d times", single)
	}
	// Primed again within the cache's time: nothing is read.
	r.PrimeScopes(context.Background(), ScopeProject, []string{a, b, c})
	if len(batches) != 1 {
		t.Fatalf("primed scopes are read again: %v", batches)
	}
	// A failed batch primes nothing: each scope reads its own, as before.
	scopedCache.Purge()
	readScopedMany = func(context.Context, string, []string) (map[string]map[string]string, error) {
		return nil, errors.New("down")
	}
	r.PrimeScopes(context.Background(), ScopeProject, []string{a})
	if got := r.ChoiceIn(context.Background(), whoEdits, Scope{Kind: ScopeProject, ID: a}); got != "members" || single != 1 {
		t.Fatalf("after a failed batch: %s, %d reads of its own", got, single)
	}
}
