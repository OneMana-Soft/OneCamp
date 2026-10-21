//go:build integration

package dgexec_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/akashc777/OneCamp/models/dgexec"
	"github.com/akashc777/OneCamp/tests/integration"
)

// Reads decode into the caller's type, First says when nothing matched, and
// writes (an upsert over a query's variable among them) commit.
func TestDgexecReadsAndWrites(t *testing.T) {
	ctx := context.Background()
	integration.SetupDgraph(t)
	type user struct {
		Uid  string `json:"uid,omitempty"`
		UUID string `json:"user_uuid,omitempty"`
		Name string `json:"user_name,omitempty"`
	}
	uids, err := dgexec.Set(ctx, []user{{Uid: "_:a", UUID: "a", Name: "ann"}, {Uid: "_:b", UUID: "b", Name: "bo"}})
	if err != nil || uids["a"] == "" || uids["b"] == "" {
		t.Fatalf("set: %v %v", uids, err)
	}
	const q = `query q($id: string) { people(func: eq(user_uuid, $id)) { uid user_uuid user_name } }`
	got, err := dgexec.First[user](ctx, q, map[string]string{"$id": "a"}, "people")
	if err != nil || got.Name != "ann" || got.Uid != uids["a"] {
		t.Fatalf("first: %+v %v", got, err)
	}
	if _, err := dgexec.First[user](ctx, q, map[string]string{"$id": "nobody"}, "people"); !errors.Is(err, dgexec.ErrNotFound) {
		t.Fatalf("an empty block is ErrNotFound: %v", err)
	}
	all, err := dgexec.Read[struct {
		People []user `json:"people"`
	}](ctx, `{ people(func: has(user_uuid), orderasc: user_name, first: 10) { user_name } }`, nil)
	if err != nil || len(all.People) != 2 || all.People[0].Name != "ann" {
		t.Fatalf("read: %+v %v", all, err)
	}

	// An upsert renames whoever its query found.
	if _, err := dgexec.Upsert(ctx, `query q($id: string) { q(func: eq(user_uuid, $id)) { who as uid } }`, map[string]string{"$id": "b"},
		user{Uid: dgexec.Var("who"), Name: "bob"}); err != nil {
		t.Fatal(err)
	}
	if b, err := dgexec.First[user](ctx, q, map[string]string{"$id": "b"}, "people"); err != nil || b.Name != "bob" {
		t.Fatalf("after the upsert: %+v %v", b, err)
	}
	// A conditional mutation whose condition fails writes nothing.
	if _, err := dgexec.Mutate(ctx, `query q($id: string) { q(func: eq(user_uuid, $id)) { who as uid } }`, map[string]string{"$id": "a"},
		dgexec.Mutation{Set: user{Uid: dgexec.Var("who"), Name: "never"}, Cond: "@if(eq(len(who), 0))"}); err != nil {
		t.Fatal(err)
	}
	if a, _ := dgexec.First[user](ctx, q, map[string]string{"$id": "a"}, "people"); a.Name != "ann" {
		t.Fatalf("a failed condition wrote: %+v", a)
	}
}

// InTxn's read and write commit together: three concurrent read-then-write
// increments of one value lose none (an aborted one runs again and reads the
// others' writes; three is what dgraphInit's three tries always cover), and
// an error from the function writes nothing.
func TestDgexecInTxnReadsWhatItWritesOver(t *testing.T) {
	ctx := context.Background()
	integration.SetupDgraph(t)
	type user struct {
		Uid  string `json:"uid,omitempty"`
		UUID string `json:"user_uuid,omitempty"`
		Name string `json:"user_name,omitempty"`
	}
	if _, err := dgexec.Set(ctx, user{Uid: "_:c", UUID: "c", Name: "0"}); err != nil {
		t.Fatal(err)
	}
	const q = `query q($id: string) { people(func: eq(user_uuid, $id)) { uid user_name } }`
	vars := map[string]string{"$id": "c"}
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- dgexec.InTxn(ctx, func(tx *dgexec.Txn) error {
				got, err := dgexec.ReadIn[struct {
					People []user `json:"people"`
				}](tx, q, vars)
				if err != nil || len(got.People) != 1 {
					return errors.New("read the counter: " + strconv.Itoa(len(got.People)))
				}
				n, _ := strconv.Atoi(got.People[0].Name)
				_, err = tx.Mutate("", nil, dgexec.Mutation{Set: user{Uid: got.People[0].Uid, Name: strconv.Itoa(n + 1)}})
				return err
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if c, err := dgexec.First[user](ctx, q, vars, "people"); err != nil || c.Name != "3" {
		t.Fatalf("after three increments: %+v %v, want 3", c, err)
	}

	refused := errors.New("refused")
	err := dgexec.InTxn(ctx, func(tx *dgexec.Txn) error {
		if _, err := tx.Mutate(`query q($id: string) { q(func: eq(user_uuid, $id)) { who as uid } }`, vars,
			dgexec.Mutation{Set: user{Uid: dgexec.Var("who"), Name: "never"}}); err != nil {
			return err
		}
		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("InTxn returned %v, want the function's error", err)
	}
	if c, _ := dgexec.First[user](ctx, q, vars, "people"); c.Name != "3" {
		t.Fatalf("a function that failed wrote: %+v", c)
	}
}
