package helpers

import (
	"encoding/xml"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every table the observability ClickHouse writes must have a retention limit, and it must not
// profile itself.
//
// WHY THIS IS A TEST. On the demo host, system.trace_log reached 455 million rows (9.6 GB) of
// query-profiler and memory samples that nothing reads, and merging them kept two cores busy all
// day, taken from the workspace running on the same machine. The otel_metrics_* tables had no TTL
// at all, because an older collector made them and its migrations only CREATE IF NOT EXISTS. Each
// is one missing element in an XML file, invisible in review, and it costs nothing until months
// later. The fix was verified against clickhouse-server:25.6 itself; this keeps it in place.

const chConfig, chUsers, chInit = "../ch-docker/config.xml", "../ch-docker/users.xml", "../ch-docker/init-db.sh"

// logsWithoutRetention may lack a TTL. crash_log holds stack traces of fatal errors, is empty on
// a healthy server, and is worth keeping for as long as it takes someone to look.
var logsWithoutRetention = map[string]bool{"crash_log": true}

// A system log section: <query_log> ... <ttl>, or an <engine> clause carrying its own TTL.
type chLog struct {
	XMLName xml.Name
	Table   string `xml:"table"`
	TTL     string `xml:"ttl"`
	Engine  string `xml:"engine"`
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestEveryClickHouseSystemLogHasARetentionLimit(t *testing.T) {
	var cfg struct {
		Items []chLog `xml:",any"`
	}
	if err := xml.Unmarshal([]byte(readFile(t, chConfig)), &cfg); err != nil {
		t.Fatalf("parse %s: %v", chConfig, err)
	}
	seen := 0
	for _, it := range cfg.Items {
		name := it.XMLName.Local
		if !strings.HasSuffix(name, "_log") || it.Table == "" {
			continue
		}
		seen++
		if logsWithoutRetention[name] {
			continue
		}
		if it.Engine != "" {
			// ClickHouse refuses to start when both are given, so the TTL must be in the engine.
			if it.TTL != "" {
				t.Errorf("%s has an <engine> and a <ttl>: ClickHouse will not start; put the TTL in the engine", name)
			}
			if !regexp.MustCompile(`(?i)\bttl\b`).MatchString(it.Engine) {
				t.Errorf("%s has a custom engine with no TTL in it, so it grows for ever", name)
			}
			continue
		}
		if strings.TrimSpace(it.TTL) == "" {
			t.Errorf("%s has no <ttl>, so it grows for ever", name)
		}
	}
	if seen < 8 {
		t.Fatalf("found only %d system logs in %s; the parse is not seeing the file", seen, chConfig)
	}
}

func TestClickHouseDoesNotProfileItself(t *testing.T) {
	users := readFile(t, chUsers)
	for _, setting := range []string{"query_profiler_real_time_period_ns", "query_profiler_cpu_time_period_ns", "memory_profiler_step"} {
		if !strings.Contains(users, "<"+setting+">0</"+setting+">") {
			t.Errorf("%s is not 0 in %s: the profiler fills system.trace_log with samples nobody reads", setting, chUsers)
		}
	}
}

// Tables the collector or the app create must each get a TTL: init-db.sh creates some, and the
// startup scripts in config.xml add one to any metrics table that exists without it.
func TestEveryTelemetryTableGetsARetentionLimit(t *testing.T) {
	cfg := readFile(t, chConfig)
	for _, table := range []string{"otel_metrics_gauge", "otel_metrics_sum", "otel_metrics_histogram",
		"otel_metrics_exponential_histogram", "otel_metrics_summary"} {
		if !strings.Contains(cfg, "ALTER TABLE default."+table+" MODIFY TTL") {
			t.Errorf("%s is never given a TTL by the startup scripts in %s", table, chConfig)
		}
	}
	if !strings.Contains(cfg, "<throw_on_error>false</throw_on_error>") {
		t.Error("a failing startup script would stop ClickHouse from starting; throw_on_error must be false")
	}
	init := readFile(t, chInit)
	creates := regexp.MustCompile(`(?s)CREATE TABLE[^;]*`).FindAllString(init, -1)
	if len(creates) == 0 {
		t.Fatalf("no CREATE TABLE found in %s", chInit)
	}
	for _, c := range creates {
		if !strings.Contains(c, "TTL ") {
			t.Errorf("a table created in %s has no TTL: %.80s", chInit, c)
		}
	}
}
