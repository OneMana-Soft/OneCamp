//go:build integration
// +build integration

package integration_test

// One host, one doc, however many make it at once (docs D-18), against a real
// graph: the host's id is @upsert, so the makes conflict, Dgraph aborts all
// but one, and each aborted make, run again in a fresh transaction, finds the
// doc that was made.
//
// Run: go test -tags=integration ./tests/integration/ -run TestOneHostGetsOneDocHoweverManyMakeIt -v

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	domainDoc "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestOneHostGetsOneDocHoweverManyMakeIt(t *testing.T) {
	ctx := context.Background()
	integration.SetupDgraph(t)
	host := uuid.NewString()
	const makes = 8
	ids := make([]string, makes)
	made := make([]bool, makes)
	errs := make([]error, makes)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < makes; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ids[i], made[i], errs[i] = domainDoc.EnsureHostedDoc(ctx, "table_row", host, uuid.NewString(), "Acme", time.Now())
		}(i)
	}
	close(start)
	wg.Wait()
	makers := 0
	for i := range ids {
		if errs[i] != nil || ids[i] == "" || ids[i] != ids[0] {
			t.Fatalf("make %d: %q %v (the first: %q)", i, ids[i], errs[i], ids[0])
		}
		if made[i] {
			makers++
		}
	}
	if makers != 1 {
		t.Errorf("%d makes said they made the doc; want one", makers)
	}
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx,
		`query q($host: string) { docs(func: eq(doc_host_uuid, $host)) { doc_uuid doc_host_kind doc_private } }`, map[string]string{"$host": host})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Docs []struct {
			UUID    string `json:"doc_uuid"`
			Kind    string `json:"doc_host_kind"`
			Private bool   `json:"doc_private"`
		} `json:"docs"`
	}
	if err := json.Unmarshal(resp.Json, &got); err != nil || len(got.Docs) != 1 || got.Docs[0].UUID != ids[0] || got.Docs[0].Kind != "table_row" || !got.Docs[0].Private {
		t.Errorf("the graph holds %s for the host; want one private doc, %s", resp.Json, ids[0])
	}
	if kind, h, err := domainDoc.HostOfDoc(ctx, ids[0]); err != nil || kind != "table_row" || h != host {
		t.Errorf("the doc's host: %q %q %v", kind, h, err)
	}
}
