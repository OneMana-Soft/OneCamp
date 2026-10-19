//go:build integration
// +build integration

package integration

// What a test may change outside its stores' contents, checked or put back
// when it ends.
//
// The stores' contents are emptied for each test (shared.go). The rest is
// shared by every test in the binary, and was before the servers were: the
// environment, the project's package-level clients, and the servers' own
// settings. So that a shared run hides nothing a fresh one would show, every
// Setup guards the test it is called for, once:
//
//   - The environment: a test that leaves a variable changed fails, naming it.
//     t.Setenv puts its own back; os.Setenv would not (no test calls it).
//   - The servers' own settings: a test that changes them fails, naming it.
//     Postgres: roles, per-role settings, ALTER SYSTEM, tablespaces,
//     replication slots, prepared transactions, and databases nobody dropped.
//     OpenSearch: cluster settings, templates, pipelines and stored scripts
//     (the reset deletes those too). Redis: CONFIG.
//
// The process's caches of what a database holds start empty for every test,
// as in a new process: the automation bot's identity, the settings, the
// booking pages and the Slack bridge's configuration. Each package keeping
// such a cache registers its Forget…ForTest with processcache from an init()
// behind the integration tag, and every Setup forgets all of them. In
// production a process has one set of databases. Here every test has its own,
// and a test that ran after another had resolved the bot posted as a user its
// database didn't have (TestAnAgentSeesOnlyWhatItsAskerCan after
// TestSlackBridgeCarriesAConversationBothWays, found by -shuffle). The harness
// imports none of those packages to do it. Packages with integration tests of
// their own import the harness, so importing them back is an import cycle,
// which stopped thirteen tests compiling on 17 Oct 2026 (processcache explains;
// helpers/integrationHarnessGuard_test.go keeps it from coming back).
//
// The project's clients are each a test's own or reach nothing once it ends:
// the database is dropped, the graph client points nowhere, the search and
// cache clients are cleared, and MQTT gets an unconnected client again. The
// 46 tests that index in passing point opensearchInit.OpenSearchClient at a
// stub of their own (an httptest server that answers "ok", holds nothing and
// is closed when the test ends), so a later test reaching it gets a refused
// connection, as before. Clocks and flags: no test replaces a clock, and the
// switches tests set (helpers.SeatLimit, CALENDAR_STORE...) are put back by
// the tests or by t.Setenv. TestTheTestsPutBackWhatTheyChange reads every
// test file for an assignment to another package's variable that its
// function doesn't undo.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/akashc777/OneCamp/tests/integration/processcache"
)

var guarded struct {
	mu    sync.Mutex
	tests map[*testing.T]bool
}

// guardTestState registers the checks above for t, once. Every Setup calls it
// first, before registering its own teardown, so the checks run after the
// test's teardowns (cleanups run last first).
func guardTestState(t *testing.T) {
	t.Helper()
	guarded.mu.Lock()
	if guarded.tests == nil {
		guarded.tests = map[*testing.T]bool{}
	}
	if guarded.tests[t] {
		guarded.mu.Unlock()
		return
	}
	guarded.tests[t] = true
	guarded.mu.Unlock()
	if !strings.Contains(t.Name(), "/") {
		forgetProcessCaches()
	}
	environ := os.Environ()
	t.Cleanup(func() {
		guarded.mu.Lock()
		delete(guarded.tests, t)
		guarded.mu.Unlock()
		if d := environDiff(environ, os.Environ()); d != "" {
			t.Errorf("the test left the environment changed: %s", d)
		}
		for _, d := range serverSettingsChanged() {
			t.Errorf("the test changed a server's own settings, which the next test would inherit: %s", d)
		}
	})
}

// forgetProcessCaches empties the caches a process keeps of what its
// databases hold: each one that a package linked into this binary registered.
func forgetProcessCaches() {
	processcache.ForgetAll()
}

// environDiff says how two environments differ, or "".
func environDiff(before, after []string) string {
	m := func(env []string) map[string]string {
		out := map[string]string{}
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			out[k] = v
		}
		return out
	}
	b, a := m(before), m(after)
	var d []string
	for k, v := range a {
		if old, ok := b[k]; !ok {
			d = append(d, k+" set")
		} else if old != v {
			d = append(d, k+" changed")
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			d = append(d, k+" unset")
		}
	}
	sort.Strings(d)
	return strings.Join(d, ", ")
}

// serverSettingsChanged lists what differs from each running shared server's
// settings when it started.
func serverSettingsChanged() []string {
	var out []string
	if s := thePostgres.current(); s != nil {
		out = append(out, s.settingsChanged()...)
	}
	if s := theOpenSearch.current(); s != nil {
		out = append(out, s.settingsChanged()...)
	}
	if s := theRedis.current(); s != nil {
		out = append(out, s.settingsChanged()...)
	}
	return out
}

// pgSettings is what a test could change on the Postgres server beyond its
// own database: roles, per-role and per-database settings, ALTER SYSTEM's
// file, tablespaces, and the databases there are.
func (s *pgServer) pgSettings() (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out := map[string]string{}
	for name, q := range map[string]string{
		"roles":                `SELECT coalesce(string_agg(rolname, ',' ORDER BY rolname), '') FROM pg_roles WHERE rolname NOT LIKE 'pg\_%'`,
		"role settings":        `SELECT count(*)::text FROM pg_db_role_setting`,
		"ALTER SYSTEM":         `SELECT count(*)::text FROM pg_file_settings WHERE sourcefile LIKE '%postgresql.auto.conf'`,
		"tablespaces":          `SELECT coalesce(string_agg(spcname, ',' ORDER BY spcname), '') FROM pg_tablespace`,
		"replication slots":    `SELECT count(*)::text FROM pg_replication_slots`,
		"prepared transaction": `SELECT count(*)::text FROM pg_prepared_xacts`,
	} {
		var v string
		if err := s.admin.QueryRowContext(ctx, q).Scan(&v); err != nil {
			return nil, err
		}
		out[name] = v
	}
	rows, err := s.admin.QueryContext(ctx, `SELECT datname FROM pg_database ORDER BY datname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var dbs []string
	s.liveMu.Lock()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			s.liveMu.Unlock()
			return nil, err
		}
		if !s.live[n] {
			dbs = append(dbs, n)
		}
	}
	s.liveMu.Unlock()
	out["databases"] = strings.Join(dbs, ",")
	return out, rows.Err()
}

func (s *pgServer) settingsChanged() []string {
	now, err := s.pgSettings()
	if err != nil {
		return []string{fmt.Sprintf("postgres: read its settings: %v", err)}
	}
	return mapDiff("postgres", s.baseline, now)
}

// osSettings is what a test could change on the OpenSearch node beyond
// indices: cluster settings, index and component templates, ingest pipelines
// and stored scripts, each by name.
func (s *openSearchServer) osSettings() (map[string]string, error) {
	out := map[string]string{}
	var cluster struct {
		Persistent map[string]any `json:"persistent"`
		Transient  map[string]any `json:"transient"`
	}
	if err := s.call("GET", "/_cluster/settings?flat_settings=true", &cluster); err != nil {
		return nil, err
	}
	out["persistent cluster settings"] = ownKeys(cluster.Persistent)
	out["transient cluster settings"] = ownKeys(cluster.Transient)
	var it struct {
		IndexTemplates []struct {
			Name string `json:"name"`
		} `json:"index_templates"`
	}
	if err := s.call("GET", "/_index_template", &it); err != nil {
		return nil, err
	}
	var names []string
	for _, x := range it.IndexTemplates {
		if !strings.HasPrefix(x.Name, ".") {
			names = append(names, x.Name)
		}
	}
	sort.Strings(names)
	out["index templates"] = strings.Join(names, ",")
	var ct struct {
		ComponentTemplates []struct {
			Name string `json:"name"`
		} `json:"component_templates"`
	}
	if err := s.call("GET", "/_component_template", &ct); err != nil {
		return nil, err
	}
	names = nil
	for _, x := range ct.ComponentTemplates {
		if !strings.HasPrefix(x.Name, ".") {
			names = append(names, x.Name)
		}
	}
	sort.Strings(names)
	out["component templates"] = strings.Join(names, ",")
	var pipelines map[string]any
	if err := s.call("GET", "/_ingest/pipeline", &pipelines); err != nil {
		return nil, err
	}
	out["ingest pipelines"] = ownKeys(pipelines)
	var state struct {
		Metadata struct {
			StoredScripts map[string]any `json:"stored_scripts"`
		} `json:"metadata"`
	}
	if err := s.call("GET", "/_cluster/state/metadata?filter_path=metadata.stored_scripts", &state); err != nil {
		return nil, err
	}
	out["stored scripts"] = ownKeys(state.Metadata.StoredScripts)
	return out, nil
}

func (s *openSearchServer) settingsChanged() []string {
	now, err := s.osSettings()
	if err != nil {
		return []string{fmt.Sprintf("opensearch: read its settings: %v", err)}
	}
	return mapDiff("opensearch", s.baseline, now)
}

// redisSettings is the server's CONFIG.
func (s *redisServer) redisSettings() (map[string]string, error) {
	c := redis.NewClient(&redis.Options{Addr: s.addr})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.ConfigGet(ctx, "*").Result()
}

func (s *redisServer) settingsChanged() []string {
	now, err := s.redisSettings()
	if err != nil {
		return []string{fmt.Sprintf("redis: read its settings: %v", err)}
	}
	return mapDiff("redis", s.baseline, now)
}

// mapDiff names the keys whose values differ.
func mapDiff(server string, before, after map[string]string) []string {
	var out []string
	for k, v := range after {
		if before[k] != v {
			out = append(out, fmt.Sprintf("%s %s: was %q, now %q", server, k, before[k], v))
		}
	}
	for k, v := range before {
		if _, ok := after[k]; !ok {
			out = append(out, fmt.Sprintf("%s %s: was %q, now gone", server, k, v))
		}
	}
	sort.Strings(out)
	return out
}

// ownKeys is a map's keys, sorted and joined, less the ones the node's
// plugins keep for themselves (named with a dot, or under plugins. or
// opendistro.), which they may write on their own while a test runs.
func ownKeys[V any](m map[string]V) string {
	out := make([]string, 0, len(m))
	for k := range m {
		if !strings.HasPrefix(k, ".") && !strings.HasPrefix(k, "plugins.") && !strings.HasPrefix(k, "opendistro.") {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
