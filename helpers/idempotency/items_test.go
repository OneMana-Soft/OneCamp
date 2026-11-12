package idempotency

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeItems keeps a batch's claims and records in memory, by their args, and
// fails where a test says.
type fakeItems struct {
	claims                         map[string]string // args -> the holder's token
	kept                           map[string]string
	failClaim, failRead, failWrite bool
}

func (f *fakeItems) claim(_ context.Context, args []string, token string) (bool, error) {
	if f.failClaim {
		return false, errors.New("redis: connection refused")
	}
	k := strings.Join(args, "|")
	if _, held := f.claims[k]; held {
		return false, nil
	}
	f.claims[k] = token
	return true, nil
}

func (f *fakeItems) read(_ context.Context, args []string) (string, bool, error) {
	if f.failRead {
		return "", false, errors.New("redis: i/o timeout")
	}
	v, ok := f.kept[strings.Join(args, "|")]
	return v, ok, nil
}

func (f *fakeItems) write(_ context.Context, args []string, value string) error {
	if f.failWrite {
		return errors.New("redis: OOM command not allowed")
	}
	f.kept[strings.Join(args, "|")] = value
	return nil
}

func (f *fakeItems) forget(_ context.Context, args []string) error {
	delete(f.kept, strings.Join(args, "|"))
	return nil
}

func (f *fakeItems) release(_ context.Context, args []string, token string) {
	if k := strings.Join(args, "|"); f.claims[k] == token {
		delete(f.claims, k)
	}
}

func withFakeItems(t *testing.T) *fakeItems {
	t.Helper()
	f := &fakeItems{claims: map[string]string{}, kept: map[string]string{}}
	was := records
	records = f
	t.Cleanup(func() { records = was })
	return f
}

// One send holds a batch's key at a time: a second meanwhile is refused as
// still on its way, and the key is free again once the first lets it go.
// Another caller's key of the same text is their own.
func TestABatchsKeyIsHeldByOneSendAtATime(t *testing.T) {
	withFakeItems(t)
	ctx := context.Background()
	first, err := OpenItems(ctx, "tok-1", "tables.fields.batch", "k1", "h1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenItems(ctx, "tok-1", "tables.fields.batch", "k1", "h1"); !errors.Is(err, ErrItemsInProgress) {
		t.Errorf("a second send while the first runs: %v, want ErrItemsInProgress", err)
	}
	if _, err := OpenItems(ctx, "tok-2", "tables.fields.batch", "k1", "h1"); err != nil {
		t.Errorf("another caller's key of the same text: %v, want their own", err)
	}
	first.Release(ctx)
	if _, err := OpenItems(ctx, "tok-1", "tables.fields.batch", "k1", "h1"); err != nil {
		t.Errorf("the key once the first send let it go: %v", err)
	}
}

// The items a batch did are recorded at once, with their answers, so the
// same batch sent again with its key knows them, and only them.
func TestABatchSentAgainKnowsTheItemsItDid(t *testing.T) {
	withFakeItems(t)
	ctx := context.Background()
	it, err := OpenItems(ctx, "tok-1", "tables.views.batch", "k1", "h1")
	if err != nil {
		t.Fatal(err)
	}
	if err := it.Mark(ctx, 0, json.RawMessage(`{"id":"a"}`)); err != nil {
		t.Fatal(err)
	}
	if err := it.Mark(ctx, 2, json.RawMessage(`{"id":"c"}`)); err != nil {
		t.Fatal(err)
	}
	it.Release(ctx)
	again, err := OpenItems(ctx, "tok-1", "tables.views.batch", "k1", "h1")
	if err != nil {
		t.Fatal(err)
	}
	a, ok0 := again.Done(0)
	_, ok1 := again.Done(1)
	c, ok2 := again.Done(2)
	if !ok0 || string(a) != `{"id":"a"}` || ok1 || !ok2 || string(c) != `{"id":"c"}` {
		t.Errorf("the batch sent again knows 0 %v %s, 1 %v, 2 %v %s; want 0 and 2 with their answers, not 1", ok0, a, ok1, ok2, c)
	}
}

// The same key with another batch is refused, as a reused Idempotency-Key
// is, and lets the key go for the batch it names.
func TestAKeySentWithAnotherBatchIsRefused(t *testing.T) {
	withFakeItems(t)
	ctx := context.Background()
	it, err := OpenItems(ctx, "tok-1", "tables.fields.batch", "k1", "h1")
	if err != nil {
		t.Fatal(err)
	}
	if err := it.Mark(ctx, 0, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	it.Release(ctx)
	if _, err := OpenItems(ctx, "tok-1", "tables.fields.batch", "k1", "h2"); !errors.Is(err, ErrItemsReused) {
		t.Fatalf("the key with another batch: %v, want ErrItemsReused", err)
	}
	if _, err := OpenItems(ctx, "tok-1", "tables.fields.batch", "k1", "h1"); err != nil {
		t.Errorf("the key's own batch after a refused reuse: %v; the refusal kept the claim", err)
	}
}

// No record, no batch: a claim or a record that can't be read refuses the
// send before anything runs, and lets any claim go; a record that didn't take
// an item says so.
func TestABatchWhoseRecordCantBeKeptDoesntRun(t *testing.T) {
	f := withFakeItems(t)
	ctx := context.Background()
	f.failClaim = true
	if _, err := OpenItems(ctx, "tok-1", "r", "k1", "h1"); !errors.Is(err, ErrItemsUnavailable) {
		t.Errorf("a claim Redis can't take: %v, want ErrItemsUnavailable", err)
	}
	f.failClaim, f.failRead = false, true
	if _, err := OpenItems(ctx, "tok-1", "r", "k1", "h1"); !errors.Is(err, ErrItemsUnavailable) {
		t.Errorf("a record Redis can't read: %v, want ErrItemsUnavailable", err)
	}
	f.failRead = false
	f.kept["tok-1|r|k2"] = "not a record"
	if _, err := OpenItems(ctx, "tok-1", "r", "k2", "h1"); !errors.Is(err, ErrItemsUnavailable) {
		t.Errorf("a record that isn't one: %v, want ErrItemsUnavailable", err)
	}
	f.failWrite = true
	if _, err := OpenItems(ctx, "tok-1", "r", "k3", "h1"); !errors.Is(err, ErrItemsUnavailable) {
		t.Errorf("a first send whose record can't be kept: %v, want ErrItemsUnavailable", err)
	}
	if _, held := f.claims["tok-1|r|k3"]; held {
		t.Error("a first send whose record can't be kept kept its claim")
	}
	f.failWrite = false
	it, err := OpenItems(ctx, "tok-1", "r", "k1", "h1")
	if err != nil {
		t.Fatalf("the key after a failed read: %v; the failure kept the claim", err)
	}
	f.failWrite = true
	if err := it.Mark(ctx, 0, json.RawMessage(`{}`)); err == nil {
		t.Error("an item the record didn't take was marked done")
	}
}

// A batch's fingerprint is its arguments: alike however they were built and
// whichever half (REST's json.Number, MCP's float64) read its numbers, and
// different for another batch.
func TestABatchsFingerprintIsItsArguments(t *testing.T) {
	a := map[string]any{"id": "t1", "key": "k1", "create": []any{map[string]any{"name": "Budget", "position": json.Number("2")}}}
	b := map[string]any{"create": []any{map[string]any{"position": 2.0, "name": "Budget"}}, "key": "k1", "id": "t1"}
	c := map[string]any{"id": "t1", "key": "k1", "create": []any{map[string]any{"name": "Budgets", "position": 2.0}}}
	if Fingerprint(a) != Fingerprint(b) {
		t.Error("the same batch built two ways has two fingerprints")
	}
	if Fingerprint(a) == Fingerprint(c) {
		t.Error("two batches share a fingerprint")
	}
}

// A send lets go of its key only while it holds it: one whose hold ran out
// leaves the hold of the send after it (V-529 (1)).
func TestASendLetsGoOnlyOfItsOwnHold(t *testing.T) {
	f := withFakeItems(t)
	ctx := context.Background()
	first, err := OpenItems(ctx, "tok-1", "r", "k1", "h1")
	if err != nil {
		t.Fatal(err)
	}
	delete(f.claims, "tok-1|r|k1") // its hold ran out
	second, err := OpenItems(ctx, "tok-1", "r", "k1", "h1")
	if err != nil {
		t.Fatalf("the send after the first's hold ran out: %v", err)
	}
	first.Release(ctx)
	if _, err := OpenItems(ctx, "tok-1", "r", "k1", "h1"); !errors.Is(err, ErrItemsInProgress) {
		t.Errorf("a send while the second applies: %v; want ErrItemsInProgress", err)
	}
	if !second.Again() {
		t.Error("the second send doesn't know the first came before it")
	}
	second.Release(ctx)
	if _, held := f.claims["tok-1|r|k1"]; held {
		t.Error("the second send's release kept its hold")
	}
}

// A first send that stopped before any item began forgets the record it kept
// at its claim: the batch corrected under the same key runs as a first send.
// A send after another, or one that recorded an item, keeps its record
// (CR-220 F1).
func TestAFirstSendThatAppliedNothingLeavesItsKeyUnused(t *testing.T) {
	f := withFakeItems(t)
	ctx := context.Background()
	refused, err := OpenItems(ctx, "tok-1", "r", "k1", "h1")
	if err != nil {
		t.Fatal(err)
	}
	refused.Forget(ctx)
	refused.Release(ctx)
	corrected, err := OpenItems(ctx, "tok-1", "r", "k1", "h2")
	if err != nil {
		t.Fatalf("the corrected batch under the key of one refused: %v; want it run", err)
	}
	if corrected.Again() {
		t.Error("the corrected batch says a send of it came before")
	}
	if err := corrected.Mark(ctx, 0, json.RawMessage(`{"id":"a"}`)); err != nil {
		t.Fatal(err)
	}
	corrected.Forget(ctx) // it recorded an item: kept
	corrected.Release(ctx)
	again, err := OpenItems(ctx, "tok-1", "r", "k1", "h2")
	if err != nil {
		t.Fatal(err)
	}
	if _, done := again.Done(0); !done || !again.Again() {
		t.Error("a send that recorded an item lost its record")
	}
	again.Forget(ctx) // a send after another: kept
	again.Release(ctx)
	if _, ok := f.kept["tok-1|r|k1"]; !ok {
		t.Error("a send after another forgot the batch's record")
	}
}

// A send whose item was applied but whose record missed it (nothing marked)
// leaves the batch's record; the resend that follows, refused at its checks,
// must keep it too (it came after another), so a third send still knows a
// send came before and reads the trash for its deletes (V-529 (1); CR-220 M1).
func TestAResendRefusedKeepsTheRecordOfTheSendBefore(t *testing.T) {
	withFakeItems(t)
	ctx := context.Background()
	first, err := OpenItems(ctx, "tok-1", "r", "k1", "h1")
	if err != nil {
		t.Fatal(err)
	}
	first.Release(ctx) // an item applied, its Mark failed: nothing marked, nothing forgotten
	resend, err := OpenItems(ctx, "tok-1", "r", "k1", "h1")
	if err != nil {
		t.Fatal(err)
	}
	if _, done := resend.Done(0); done || !resend.Again() {
		t.Fatalf("the resend: done %v, again %v; want no item done and a send before", done, resend.Again())
	}
	resend.Forget(ctx) // refused at its checks
	resend.Release(ctx)
	third, err := OpenItems(ctx, "tok-1", "r", "k1", "h1")
	if err != nil {
		t.Fatal(err)
	}
	defer third.Release(ctx)
	if !third.Again() {
		t.Error("a resend refused at its checks forgot the record of the send before it: the third send doesn't know one came before")
	}
}
