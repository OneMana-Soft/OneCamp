//go:build integration

package businness

import (
	"bytes"
	"context"
	"errors"
	"testing"

	domain "github.com/akashc777/OneCamp/domain/Doc"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// repairdocs restores each kind of damaged doc from the right source, leaves
// healthy and open docs alone, and a second run changes nothing.
//
// Run: go test -tags=integration ./business/Doc/ -run TestRepairDocs -v
func TestRepairDocsRestoresDamagedDocsOnce(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)

	const full = "<h1>Launch plan</h1><p>Ship on Thursday, after the review with design and legal.</p><div data-type=\"table-embed\" data-table-id=\"t1\"></div>"
	const old = "<h1>Launch plan</h1><p>An earlier version from the history, long enough to matter.</p>"
	short := "<p>Plan</p>"
	ids := map[string]string{}
	for _, k := range []string{"stateful", "historied", "lost", "short", "healthy", "open"} {
		ids[k] = uuid.NewString()
	}
	doc := func(id, body string, extra map[string]any) map[string]any {
		m := map[string]any{"uid": "_:" + id, "dgraph.type": "Doc", "doc_uuid": id, "doc_title": "t", "doc_body": body}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	dg.Mutate(t, []map[string]any{
		doc(ids["stateful"], collabErrorBody, map[string]any{"doc_yjs_state": "c3RhdGVmdWw=", "doc_yjs_body_hash": "older"}),
		doc(ids["historied"], collabErrorBody, nil),
		doc(ids["lost"], collabErrorBody, nil),
		doc(ids["short"], short, map[string]any{"doc_yjs_state": "c2hvcnQ=", "doc_yjs_body_hash": collabBodyHash(short)}),
		doc(ids["healthy"], full, map[string]any{"doc_yjs_state": "aGVhbHRoeQ==", "doc_yjs_body_hash": collabBodyHash(full)}),
		doc(ids["open"], collabErrorBody, map[string]any{"doc_yjs_state": "b3Blbg==", "doc_yjs_body_hash": "x"}),
	})

	render := func(_ context.Context, id, state string) (string, bool, error) {
		if id == ids["open"] {
			return "", true, nil
		}
		if state == "" {
			return "", false, nil
		}
		return full, false, nil
	}
	history := func(_ context.Context, id string) (string, error) {
		if id == ids["historied"] {
			return old, nil
		}
		return "", errors.New("no earlier version")
	}
	writes := 0
	write := func(ctx context.Context, id, body, state string) error {
		writes++
		d := &dgraphStruct.DgraphDoc{Uid: "uid(doc)", Uuid: id, Body: body}
		if state != "" {
			d.YjsState, d.YjsBodyHash = state, collabBodyHash(body)
		}
		_, err := domain.CreateOrUpdateDgraphDoc(ctx, d)
		return err
	}

	var out bytes.Buffer
	dry, err := repairDocs(ctx, false, &out, render, history, write)
	if err != nil {
		t.Fatal(err)
	}
	if writes != 0 || dry.FromState != 2 || dry.FromSnapshot != 1 || dry.SkippedOpen != 1 || len(dry.Unrecoverable) != 1 {
		t.Fatalf("dry run: writes %d, %s\n%s", writes, dry, out.String())
	}

	rep, err := repairDocs(ctx, true, &out, render, history, write)
	if err != nil {
		t.Fatal(err)
	}
	if writes != 3 || rep.FromState != 2 || rep.FromSnapshot != 1 || rep.Placeholder != 4 || rep.ShortBody != 1 {
		t.Fatalf("apply: writes %d, %s\n%s", writes, rep, out.String())
	}
	for k, want := range map[string]string{"stateful": full, "short": full, "historied": old, "healthy": full, "lost": collabErrorBody, "open": collabErrorBody} {
		got, err := domain.GetCollabDocByUUID(ctx, ids[k])
		if err != nil || got == nil || got.Body != want {
			t.Fatalf("%s: body %+v, err %v", k, got, err)
		}
	}
	// Rebuilt from its state, a doc opens from that state again.
	if got, _ := domain.GetCollabDocByUUID(ctx, ids["stateful"]); got.YjsBodyHash != collabBodyHash(full) || got.YjsState != "c3RhdGVmdWw=" {
		t.Fatalf("stateful doc state %q hash %q", got.YjsState, got.YjsBodyHash)
	}

	again, err := repairDocs(ctx, true, &out, render, history, write)
	if err != nil {
		t.Fatal(err)
	}
	if writes != 3 || again.FromState != 0 || again.FromSnapshot != 0 {
		t.Fatalf("second run wrote: writes %d, %s\n%s", writes, again, out.String())
	}
}
