package loggerInit

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"go.opentelemetry.io/otel/attribute"
	logglobal "go.opentelemetry.io/otel/log/global"
	lognoop "go.opentelemetry.io/otel/log/noop"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// A link a vendor signs, or a key or token in a query, is a working
// credential, and both sinks keep a line: otel_logs for 30 days, and the
// container's log. So however the server logs a line (helpers' Log*,
// MessageLogs, slog, a logger made With attributes, or the log package), each
// sink gets it with every URL's query and fragment taken off, and the rest of
// the URL kept, so the line still says which call it was.
func TestALogLineReachesNeitherSinkWithAURLQuery(t *testing.T) {
	stdout, otelLogs := initLoggerForTest(t)

	signed := "https://files.vendor.test/a/report.pdf?X-Amz-Signature=5ee3c0ffee&X-Amz-Credential=AKIAEXAMPLE#page=2"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:1/1/members/me/boards?key=0123abcd&token=ATTA9876", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, callErr := http.DefaultClient.Do(req)
	if callErr == nil {
		t.Fatal("nothing listens on port 1")
	}

	helpers.LogWarnWithContext(ctx, "fetch %s failed: %v", signed, callErr)
	helpers.LogWithContext(ctx, "INFO", "fetched", signed)
	helpers.MessageLogs.ErrorLog.Printf("avatar gone url=%s", signed)
	slog.WarnContext(ctx, "upload failed", "err", callErr, slog.Group("file", "url", signed))
	slog.Default().With("link", signed, slog.Group("file", "url", signed)).WarnContext(ctx, "upload kept")
	log.Printf("fetch %s", signed)

	for sink, lines := range map[string][]string{"stdout": linesOf(t, stdout), "otel_logs": otelLogs.all()} {
		all := strings.Join(lines, "\n")
		if len(lines) != 6 {
			t.Errorf("%s has %d lines, want 6:\n%s", sink, len(lines), all)
		}
		for _, secret := range []string{"X-Amz-Signature", "AKIAEXAMPLE", "page=2", "0123abcd", "ATTA9876"} {
			if strings.Contains(all, secret) {
				t.Errorf("%s keeps %q:\n%s", sink, secret, all)
			}
		}
		for _, call := range []string{"https://files.vendor.test/a/report.pdf", "http://127.0.0.1:1/1/members/me/boards"} {
			if !strings.Contains(all, call) {
				t.Errorf("%s no longer says which call it was (%s):\n%s", sink, call, all)
			}
		}
	}
}

// Most lines hold no URL, and they are passed on as they came: the clean costs
// them a scan, and no allocation unless an attribute is a LogValuer (the scan
// resolves it, as each sink does). BenchmarkALogLine measures what it adds to
// a line of each kind.
func TestALineWithoutAURLIsPassedOnAsItCame(t *testing.T) {
	r := slog.NewRecord(time.Now(), slog.LevelWarn, "Import job 6f1c failed: context deadline exceeded", 0)
	r.AddAttrs(slog.String("args", "job=6f1c"), slog.Any("err", errors.New("no rows")), slog.Group("chunk", slog.Int("n", 3)))
	if allocs := testing.AllocsPerRun(200, func() { _ = withoutURLQueries(r) }); allocs != 0 {
		t.Errorf("the clean allocates %.0f times on a line without a URL, want 0", allocs)
	}
	if got := withoutURLQueries(r); got.Message != r.Message || got.NumAttrs() != r.NumAttrs() {
		t.Errorf("a line without a URL changed: %q with %d attributes", got.Message, got.NumAttrs())
	}
}

func BenchmarkALogLine(b *testing.B) {
	for name, message := range map[string]string{
		"without a URL": "Import job 6f1c failed: context deadline exceeded",
		"a signed link": "fetch https://files.vendor.test/a/report.pdf?X-Amz-Signature=5ee3c0ffee failed",
	} {
		r := slog.NewRecord(time.Now(), slog.LevelWarn, message, 0)
		r.AddAttrs(slog.String("args", "job=6f1c"))
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = withoutURLQueries(r)
			}
		})
	}
}

// initLoggerForTest runs InitLogger as the server does, with its stdout in a
// file and its OTel provider exporting to otelLogs, and puts every global it
// sets back when the test ends.
func initLoggerForTest(t *testing.T) (stdout *os.File, otelLogs *exportedLines) {
	t.Helper()
	stdout, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	otelLogs = &exportedLines{}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(otelLogs)))
	logger, messages, slogDefault := helpers.Logger, helpers.MessageLogs, slog.Default()
	logOut, logFlags, osStdout := log.Writer(), log.Flags(), os.Stdout
	t.Cleanup(func() {
		helpers.Logger, helpers.MessageLogs = logger, messages
		slog.SetDefault(slogDefault)
		log.SetOutput(logOut)
		log.SetFlags(logFlags)
		logglobal.SetLoggerProvider(lognoop.NewLoggerProvider())
		_ = provider.Shutdown(context.Background())
	})

	os.Stdout = stdout
	logglobal.SetLoggerProvider(provider)
	InitLogger()
	os.Stdout = osStdout
	return stdout, otelLogs
}

// linesOf is the lines written to f.
func linesOf(t *testing.T, f *os.File) []string {
	t.Helper()
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return strings.FieldsFunc(string(b), func(r rune) bool { return r == '\n' })
}

// exportedLines is an OTel exporter keeping what would reach otel_logs: each
// record's body and attributes, as text.
type exportedLines struct {
	mu    sync.Mutex
	lines []string
}

func (e *exportedLines) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range records {
		var b strings.Builder
		b.WriteString(r.Body().String())
		r.WalkAttributes(func(kv attribute.KeyValue) bool {
			b.WriteString(" " + string(kv.Key) + "=" + kv.Value.String())
			return true
		})
		e.lines = append(e.lines, b.String())
	}
	return nil
}

func (e *exportedLines) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.lines...)
}

func (e *exportedLines) Shutdown(context.Context) error   { return nil }
func (e *exportedLines) ForceFlush(context.Context) error { return nil }
