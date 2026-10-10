//go:build integration
// +build integration

package integration

// OpenSearch side of the harness: the version production runs
// (final-compose.yml), one node, security off, so a test can ask the real
// thing how a query, a mapping or an update script behaves. Most tests that
// index in passing point the client at a stub that answers "ok"; this is for
// the ones about what the index then holds.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
)

// OpenSearchEnv carries the live OpenSearch for one test.
type OpenSearchEnv struct {
	container testcontainers.Container
	URL       string
}

// SetupOpenSearch starts OpenSearch and points opensearchInit.OpenSearchClient
// at it, as the server does at boot.
func SetupOpenSearch(t *testing.T) *OpenSearchEnv {
	t.Helper()
	ensureLoggers()
	ctx := context.Background()

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "opensearchproject/opensearch:3.6.0",
			ExposedPorts: []string{"9200/tcp"},
			Env: map[string]string{
				"discovery.type":              "single-node",
				"DISABLE_SECURITY_PLUGIN":     "true",
				"DISABLE_INSTALL_DEMO_CONFIG": "true",
				"OPENSEARCH_JAVA_OPTS":        "-Xms512m -Xmx512m",
			},
			WaitingFor: wait.ForHTTP("/_cluster/health").WithPort("9200/tcp").WithStartupTimeout(240 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start opensearch container: %v", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("opensearch host: %v", err)
	}
	port, err := c.MappedPort(ctx, "9200")
	if err != nil {
		t.Fatalf("opensearch port: %v", err)
	}
	url := fmt.Sprintf("http://%s:%s", host, port.Port())

	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{url}}})
	if err != nil {
		t.Fatalf("opensearch client: %v", err)
	}
	opensearchInit.OpenSearchClient = client

	env := &OpenSearchEnv{container: c, URL: url}
	t.Cleanup(env.Close)
	return env
}

// Close terminates the container and clears the global client.
func (e *OpenSearchEnv) Close() {
	if e == nil || e.container == nil {
		return
	}
	opensearchInit.OpenSearchClient = nil
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = e.container.Terminate(ctx)
}
