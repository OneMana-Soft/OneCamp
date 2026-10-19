//go:build integration
// +build integration

package integration_test

// Work a test hands off and doesn't wait for finds the server's clients as the
// harness leaves them, never nil.
//
// On 18 Oct 2026 a `go mqttBusiness.PublishTaskComment(...)` started by
// TestATasksEmailsReachOnlyItsReaders ran after the test had put
// mqttInit.MqttClient back to nil. It dereferenced nil and the panic took the
// binary down, so the twelve tests after it never ran. Every cleanup now
// leaves a client that reaches nothing (UseMqttClient, UseOpenSearchClient,
// the Dgraph and OpenSearch Close). helpers/integrationHarnessGuard_test.go
// fails on any integration file that sets one of these back to nil.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	mqttInit "github.com/akashc777/OneCamp/initializers/mqttInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestWorkLeftRunningAfterATestFindsNoNilClient(t *testing.T) {
	t.Run("a test with clients of its own", func(t *testing.T) {
		integration.StubMqttClient(t)
		search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{}`))
		}))
		t.Cleanup(search.Close)
		c, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
		if err != nil {
			t.Fatal(err)
		}
		integration.UseOpenSearchClient(t, c)
	})

	// That test's cleanups have run. A goroutine it started would see this.
	if mqttInit.MqttClient == nil || opensearchInit.OpenSearchClient == nil || dgraphInit.DgraphClient == nil {
		t.Fatalf("a client is nil after a test ended: mqtt %v, search %v, graph %v",
			mqttInit.MqttClient == nil, opensearchInit.OpenSearchClient == nil, dgraphInit.DgraphClient == nil)
	}
	// The publish that took the binary down returns, publishing nowhere.
	mqttBusiness.PublishTaskComment(&mqttStruct.MqttTaskComment{CommentUuid: uuid.NewString(), TaskUuid: uuid.NewString()}, uuid.NewString())
	ctx := context.Background()
	if _, err := opensearchInit.OpenSearchClient.Cluster.Health(ctx, nil); err == nil {
		t.Error("the search client left between tests reaches a node")
	}
	if _, err := dgraphInit.DgraphClient.NewReadOnlyTxn().Query(ctx, `{ q(func: has(dgraph.type), first: 1) { uid } }`); err == nil {
		t.Error("the graph client left between tests reaches a graph")
	}
}
