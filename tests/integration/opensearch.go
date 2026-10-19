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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
)

// The server's search client in the integration binary is never nil, for the
// reason mqtt.go gives for MQTT: code under test indexes from goroutines it
// doesn't wait for (go domain.UpdateAttachmentInOpenSearch, ...), and one that
// ran after a test had put the client back to nil would dereference it and
// take the whole binary down. Between tests the client is one that reaches
// nothing: every call fails at once with a refused connection, which is what
// production's own nil checks answer too ("search is not connected").
func init() {
	if opensearchInit.OpenSearchClient == nil {
		opensearchInit.OpenSearchClient = unreachableOpenSearch()
	}
}

var nowhereSearch struct {
	once   sync.Once
	client *opensearchapi.Client
}

// unreachableOpenSearch is a search client that reaches nothing: port 1, no
// retries.
func unreachableOpenSearch() *opensearchapi.Client {
	nowhereSearch.once.Do(func() {
		c, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{
			Addresses: []string{"http://127.0.0.1:1"}, DisableRetry: true,
		}})
		if err != nil {
			panic(fmt.Sprintf("integration: a search client that reaches nothing: %v", err))
		}
		nowhereSearch.client = c
	})
	return nowhereSearch.client
}

// UseOpenSearchClient makes c the server's search client for the rest of the
// test (a stub of the test's own, say). When the test ends, once the work it
// handed off has settled, the client reaches nothing again; it is never nil.
func UseOpenSearchClient(t *testing.T, c *opensearchapi.Client) {
	t.Helper()
	guardTestState(t)
	ensureLoggers()
	opensearchInit.OpenSearchClient = c
	t.Cleanup(func() { opensearchInit.OpenSearchClient = unreachableOpenSearch() })
	settleBeforeTeardown(t)
}

// OpenSearchEnv carries the live OpenSearch for one test.
type OpenSearchEnv struct {
	URL string
}

// SetupOpenSearch gives the test an OpenSearch holding what a new node holds,
// and points opensearchInit.OpenSearchClient at it, as the server does at
// boot. The node is the binary's one (shared.go): what the tests before made
// on it, their indices and retention policies, is deleted first.
func SetupOpenSearch(t *testing.T) *OpenSearchEnv {
	t.Helper()
	guardTestState(t)
	ensureLoggers()

	srv, err := theOpenSearch.prepared(startOpenSearch, func(s *openSearchServer) error {
		if err := s.reset(); err != nil {
			return fmt.Errorf("opensearch: delete what the tests before made: %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%v", err)
	}

	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{srv.url}}})
	if err != nil {
		t.Fatalf("opensearch client: %v", err)
	}
	opensearchInit.OpenSearchClient = client

	env := &OpenSearchEnv{URL: srv.url}
	t.Cleanup(env.Close)
	settleBeforeTeardown(t)
	return env
}

// Close leaves the server's search client reaching nothing, never nil.
func (e *OpenSearchEnv) Close() {
	if e == nil {
		return
	}
	opensearchInit.OpenSearchClient = unreachableOpenSearch()
}

// openSearchServer is the binary's node, the indices it held when it was new,
// which no test made (plugins' own), and its settings then (teststate.go).
type openSearchServer struct {
	url      string
	fresh    map[string]bool
	baseline map[string]string
	http     *http.Client
}

var theOpenSearch shared[openSearchServer]

// startOpenSearch starts the binary's node.
func startOpenSearch() (*openSearchServer, func(), error) {
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
				// A node for one test run, on the host's disk: past 95% full
				// it would refuse to create an index ("cluster create-index
				// blocked"), and every test that makes one failed on a full
				// workstation, whatever it tested.
				"cluster.routing.allocation.disk.threshold_enabled": "false",
			},
			WaitingFor: wait.ForHTTP("/_cluster/health").WithPort("9200/tcp").WithStartupTimeout(240 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("start opensearch container: %w", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		terminate(c)
		return nil, nil, fmt.Errorf("opensearch host: %w", err)
	}
	port, err := c.MappedPort(ctx, "9200")
	if err != nil {
		terminate(c)
		return nil, nil, fmt.Errorf("opensearch port: %w", err)
	}
	s := &openSearchServer{url: fmt.Sprintf("http://%s:%s", host, port.Port()), http: &http.Client{Timeout: time.Minute}}
	names, err := s.indices()
	if err != nil {
		terminate(c)
		return nil, nil, fmt.Errorf("opensearch: list a new node's indices: %w", err)
	}
	s.fresh = map[string]bool{}
	for _, n := range names {
		s.fresh[n] = true
	}
	if s.baseline, err = s.osSettings(); err != nil {
		terminate(c)
		return nil, nil, fmt.Errorf("opensearch: read a new node's settings: %w", err)
	}
	return s, func() { terminate(c) }, nil
}

// reset deletes what tests made: every index a new node doesn't have
// (system indices, named with a dot, are the plugins' and stay), then every
// index management policy, of which a new node has none
// (ConnectOpenSearch installs the audit log's).
func (s *openSearchServer) reset() error {
	names, err := s.indices()
	if err != nil {
		return err
	}
	var made []string
	for _, n := range names {
		if !s.fresh[n] && !strings.HasPrefix(n, ".") {
			made = append(made, n)
		}
	}
	for len(made) > 0 {
		batch := made
		if len(batch) > 50 {
			batch = made[:50]
		}
		made = made[len(batch):]
		if err := s.call(http.MethodDelete, "/"+url.PathEscape(strings.Join(batch, ","))+"?expand_wildcards=all", nil); err != nil {
			return err
		}
	}
	var policies struct {
		Policies []struct {
			ID string `json:"_id"`
		} `json:"policies"`
	}
	if err := s.call(http.MethodGet, "/_plugins/_ism/policies?size=1000", &policies); err != nil {
		return err
	}
	for _, p := range policies.Policies {
		if err := s.call(http.MethodDelete, "/_plugins/_ism/policies/"+url.PathEscape(p.ID), nil); err != nil {
			return err
		}
	}
	// Templates, pipelines and stored scripts a test made (none does today:
	// teststate.go fails the test that would).
	now, err := s.osSettings()
	if err != nil {
		return err
	}
	for kind, path := range map[string]string{
		"index templates":     "/_index_template/",
		"component templates": "/_component_template/",
		"ingest pipelines":    "/_ingest/pipeline/",
		"stored scripts":      "/_scripts/",
	} {
		was := map[string]bool{}
		for _, n := range strings.Split(s.baseline[kind], ",") {
			was[n] = true
		}
		for _, n := range strings.Split(now[kind], ",") {
			if n != "" && !was[n] {
				if err := s.call(http.MethodDelete, path+url.PathEscape(n), nil); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// indices names every index on the node, hidden and closed ones too.
func (s *openSearchServer) indices() ([]string, error) {
	var rows []struct {
		Index string `json:"index"`
	}
	if err := s.call(http.MethodGet, "/_cat/indices?format=json&h=index&expand_wildcards=all", &rows); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r.Index)
	}
	return names, nil
}

// call sends one request to the node and decodes its answer into out, if
// given. Not found is no error: what a reset deletes may already be gone, and
// a node no policy was ever made on has none to list.
func (s *openSearchServer) call(method, path string, out any) error {
	req, err := http.NewRequest(method, s.url+path, nil)
	if err != nil {
		return err
	}
	res, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	switch {
	case res.StatusCode == http.StatusNotFound:
		return nil
	case res.StatusCode/100 != 2:
		return fmt.Errorf("%s %s: %d %s", method, path, res.StatusCode, body)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("%s %s answer: %w", method, path, err)
		}
	}
	return nil
}
