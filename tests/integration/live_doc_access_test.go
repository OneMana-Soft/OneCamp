//go:build integration
// +build integration

package integration_test

// Does live access to a doc follow its sharing, at the broker?
//
// The broker asks the server about each subscription to a doc's topic (its
// comments, and the meta events open tabs act on), and never again about one
// a client holds. So someone a change took a doc from (made Restricted, moved
// out of a space, a grant removed) kept getting its new comments in any app
// already open, until it reconnected. A change that takes a doc from anyone
// now has the broker take its topic off every client whose person can't read
// it, by the rule the broker asks on a subscription (business/Doc ReadsDoc).
//
// Run: go test -tags=integration ./tests/integration/ -run TestLiveDocTopic -v

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/services/collab"
)

func TestLiveDocTopicEndsForSomeoneTheDocIsTakenFrom(t *testing.T) {
	_, dg, broker := liveStores(t)
	ctx := context.Background()

	// The change rewrites the doc's search entry and closes its live sockets:
	// both answered here, so only the broker is real.
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[],"result":"updated","_id":"x"}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	savedLive := collab.Live
	collab.Live = &socketLog{asked: map[string][][]string{}}
	t.Cleanup(func() { collab.Live = savedLive })

	// Alice's plan is open to the workspace; Cy is also given it by name. Bob
	// reads it as anyone in the workspace does.
	alice, bob, cy := uuid.NewString(), uuid.NewString(), uuid.NewString()
	doc := uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:alice", "dgraph.type": "User", "user_uuid": alice, "user_name": "alice"},
		{"uid": "_:bob", "dgraph.type": "User", "user_uuid": bob, "user_name": "bob"},
		{"uid": "_:cy", "dgraph.type": "User", "user_uuid": cy, "user_name": "cy"},
		{"uid": "_:doc", "dgraph.type": "Doc", "doc_uuid": doc, "doc_title": "Q4 plan",
			"doc_private": false, "doc_public_comment": false, "doc_deleted_at": "0001-01-01T00:00:00Z",
			"doc_created_by": map[string]any{"uid": "_:alice"}, "doc_owner": map[string]any{"uid": "_:alice"},
			"doc_reading_users": []map[string]any{{"uid": "_:cy"}}},
	})
	topic := helpers.GetMqttTopicForDoc(doc)

	// Bob's tab and Cy's, as the web app opens a doc.
	bobs, _ := broker.Connect(t, "c_dev1_"+bob[:8]+"_ab12", bob)
	bobs.Subscribe(t, topic)
	cys, _ := broker.Connect(t, "c_dev2_"+cy[:8]+"_cd34", cy)
	cys.Subscribe(t, topic)

	// Alice makes it Restricted: Bob can't read it now; Cy, named, still can.
	view, private := accessBusiness.LevelView, true
	if _, err := accessBusiness.Apply(ctx, accessBusiness.Subject{UserUID: uids["alice"], UserUUID: alice},
		accessBusiness.Change{Resource: accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: doc}, General: &view, Private: &private}); err != nil {
		t.Fatalf("Restricted: %v", err)
	}

	// The broker takes the doc's topic off Bob's client, and leaves Cy's.
	var held []string
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if held = broker.Subscriptions(t, bobs.ID); !slices.Contains(held, topic) {
			break
		}
	}
	if slices.Contains(held, topic) {
		t.Fatalf("Bob's client still holds the doc's topic: %v", held)
	}
	if got := broker.Subscriptions(t, cys.ID); !slices.Contains(got, topic) {
		t.Fatalf("Cy, who still reads it, lost the doc's topic: %v", got)
	}

	// The next comment reaches Cy, and not Bob.
	drain(bobs)
	drain(cys)
	publish(t, topic, `{"type":0,"data":{"doc_comment_text":"the numbers for Q4"}}`)
	if !receives(cys, topic, "the numbers for Q4", 5*time.Second) {
		t.Fatal("Cy, still a reader, didn't get the comment")
	}
	if receives(bobs, topic, "the numbers for Q4", 1500*time.Millisecond) {
		t.Fatal("Bob, the doc taken from him, still got the comment")
	}
}
