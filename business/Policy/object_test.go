package business

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// shape is a test object policy's value.
type shape struct {
	People []string `json:"people"`
	Weeks  int      `json:"weeks"`
}

var testShape = ObjectPolicy{Key: "test.shape", Owner: "test-panel"}

// withShape is withScopes plus an object policy projects keep, set only by
// its own panel, and a count of the scope reads.
func withShape(t *testing.T, s *store, sc scopedStore) (*Registry, *int) {
	t.Helper()
	r := withScopes(t, s, sc)
	reads := 0
	inner := readScoped
	readScoped = func(ctx context.Context, sp Scope) (map[string]string, error) {
		reads++
		return inner(ctx, sp)
	}
	oldOwned := ownedSpecs
	t.Cleanup(func() { ownedSpecs = oldOwned })
	ownedSpecs = append(append([]spec(nil), oldOwned...),
		spec{key: testShape.Key, kind: KindObject, scopes: []string{ScopeProject}, owner: testShape.Owner, label: "A shape"})
	return r, &reads
}

// An object policy is kept for a scope by its own panel only: the general
// scoped setter refuses it (so a project's policy route can't write around
// the panel's checks), another panel is refused, and what isn't one JSON
// object of at most MaxObjectBytes is refused. Each change is recorded.
func TestAnObjectPolicyIsSetOnlyByItsPanel(t *testing.T) {
	s := &store{values: map[string]string{}}
	r, _ := withShape(t, s, scopedStore{})
	ctx := context.Background()
	project := Scope{Kind: ScopeProject, ID: uuid.NewString()}
	by := Changer{UserID: uuid.New(), Email: "ana@acme.test"}
	value := json.RawMessage(`{"people": ["a"], "weeks": 2}`)
	if _, err := r.SetIn(ctx, testShape.Key, project, value, by); err == nil {
		t.Error("the general setter set an owned object policy")
	}
	if err := r.SetObjectIn(ctx, "another-panel", testShape, project, value, by); !errors.Is(err, ErrNotOwner) {
		t.Errorf("another panel: %v", err)
	}
	for _, bad := range []string{`[1]`, `"x"`, `null`, `{"people": ["` + strings.Repeat("a", MaxObjectBytes) + `"]}`, `{`} {
		if err := r.SetObjectIn(ctx, testShape.Owner, testShape, project, json.RawMessage(bad), by); !errors.Is(err, ErrBadValue) {
			t.Errorf("%.20s: %v", bad, err)
		}
	}
	if err := r.SetObjectIn(ctx, testShape.Owner, testShape, project, value, by); err != nil {
		t.Fatal(err)
	}
	if len(s.records) != 1 {
		t.Fatalf("records %v, want the one change", s.records)
	}
	var got shape
	if set, err := r.ObjectIn(ctx, testShape, project, &got); !set || err != nil || got.Weeks != 2 || len(got.People) != 1 {
		t.Fatalf("read back: %v %v %+v", set, err, got)
	}
}

// An object policy is read through its scope's one cached read: a scope whose
// triage switch was just read answers its rota with no store call, and a
// scope that keeps none answers "not set".
func TestAnObjectPolicyIsReadFromItsScopesOneRead(t *testing.T) {
	s := &store{values: map[string]string{}}
	project := Scope{Kind: ScopeProject, ID: uuid.NewString()}
	r, reads := withShape(t, s, scopedStore{project.cacheKey(): {limitHard.Key: "true", testShape.Key: `{"people":["a","b"],"weeks":1}`}})
	ctx := context.Background()
	if !r.BoolIn(ctx, limitHard, project) {
		t.Fatal("the switch")
	}
	var got shape
	if set, err := r.ObjectIn(ctx, testShape, project, &got); !set || err != nil || len(got.People) != 2 {
		t.Fatalf("%v %v %+v", set, err, got)
	}
	if *reads != 1 {
		t.Errorf("%d scope reads, want 1: the object comes from the read the switch made", *reads)
	}
	var none shape
	if set, err := r.ObjectIn(ctx, testShape, Scope{Kind: ScopeProject, ID: uuid.NewString()}, &none); set || err != nil {
		t.Errorf("a scope that keeps none: %v %v", set, err)
	}
}
