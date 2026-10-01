package models

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// captureExporter keeps the records the OTel pipeline would have shipped, so a test can inspect
// exactly what leaves the process.
type captureExporter struct{ records []sdklog.Record }

func (e *captureExporter) Export(_ context.Context, records []sdklog.Record) error {
	for _, r := range records {
		e.records = append(e.records, r.Clone()) // the SDK forbids retaining the slice
	}
	return nil
}
func (e *captureExporter) Shutdown(context.Context) error   { return nil }
func (e *captureExporter) ForceFlush(context.Context) error { return nil }

// renderValue flattens an OTel log value, following groups, so a substring check cannot be fooled
// by nesting.
func renderValue(v otellog.Value) string {
	if kvs := v.AsMap(); len(kvs) > 0 {
		var b strings.Builder
		for _, kv := range kvs {
			b.WriteString(" " + kv.Key + "=" + renderValue(kv.Value))
		}
		return b.String()
	}
	if items := v.AsSlice(); len(items) > 0 {
		var b strings.Builder
		for _, item := range items {
			b.WriteString(" " + renderValue(item))
		}
		return b.String()
	}
	return v.AsString()
}

// renderRecord flattens a whole exported record: body plus every attribute, however nested.
func renderRecord(r sdklog.Record) string {
	var b strings.Builder
	b.WriteString(r.Body().AsString())
	r.WalkAttributes(func(kv otellog.KeyValue) bool {
		b.WriteString(" " + kv.Key + "=" + renderValue(kv.Value))
		return true
	})
	return b.String()
}

// findAttr returns the named top-level attribute of an exported record.
func findAttr(r sdklog.Record, key string) (otellog.Value, bool) {
	var found otellog.Value
	var ok bool
	r.WalkAttributes(func(kv otellog.KeyValue) bool {
		if kv.Key == key {
			found, ok = kv.Value, true
			return false
		}
		return true
	})
	return found, ok
}

// THE SINK THAT LEAVES THE MACHINE.
//
// The stdout tests prove slog's JSON handler resolves LogValuer. This proves it for the OTel
// bridge, which is the one that matters most: those records are batched and shipped to a collector
// that typically retains them longer than the database, is readable by people who cannot read the
// database, and is often billed by volume.
//
// otelslog's handler.go documents that it resolves slog.KindLogValuer, and line 480 does exactly
// that. This asserts the behaviour instead of trusting the read, and it will fail if a dependency
// bump ever changes it — at which point a fully hydrated UserInfo, including other people's names
// and profile keys, would silently start flowing to the collector again.
func TestUserInfoIsSummarisedOnTheOtelSinkToo(t *testing.T) {
	exp := &captureExporter{}
	provider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)),
	)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	// The same bridge loggerInit installs, pointed at our exporter rather than the global provider.
	logger := slog.New(otelslog.NewHandler("onecamp-backend",
		otelslog.WithLoggerProvider(provider)))

	logger.LogAttrs(context.Background(), slog.LevelError, "controllers/CreateChannel failed",
		slog.Any("user_info", hydratedUserInfo(t)))

	if len(exp.records) != 1 {
		t.Fatalf("expected 1 exported record, got %d", len(exp.records))
	}
	out := renderRecord(exp.records[0])

	// The exact shape, asserted by key set rather than by absence of particular strings.
	//
	// This is the assertion that cannot go vacuous. Without LogValue the bridge sends the whole
	// struct through convert.go's reflect.Struct branch, which yields a single
	// log.StringValue(fmt.Sprintf("%+v", u)) — so this stops being a map and the test fails. It
	// also fails if someone adds a field to the group, which is the review moment worth forcing:
	// anything added here is added to every log line the request emits, on a sink billed by volume.
	attr, ok := findAttr(exp.records[0], "user_info")
	if !ok {
		t.Fatalf("no user_info attribute on the exported record:\n%s", out)
	}
	group := attr.AsMap()
	if len(group) == 0 {
		t.Fatalf("user_info reached the collector as a flat %%+v string rather than a resolved "+
			"group, meaning LogValue was not applied:\n%s", out)
	}
	gotKeys := make([]string, 0, len(group))
	for _, kv := range group {
		gotKeys = append(gotKeys, kv.Key)
	}
	wantKeys := []string{"uuid", "is_admin", "is_external", "is_bot"}
	if !slices.Equal(gotKeys, wantKeys) {
		t.Errorf("user_info group keys = %v, want %v — the set of fields shipped per log line "+
			"changed", gotKeys, wantKeys)
	}

	// Still attributable — the reason for logging the user at all is preserved.
	if !strings.Contains(out, "3f2504e0-4f89-11d3-9a0c-0305e82c3301") {
		t.Errorf("the user's uuid did not reach the OTel record; failures would be unattributable:\n%s", out)
	}

	// And nothing that does not belong off-box went with it.
	//
	// Note the asymmetry with the stdout test, which is worth knowing before trusting either in
	// isolation: on this sink a struct is rendered by %+v, which prints pointer ADDRESSES. The
	// channel, project, team and profile-key fields are all pointers, so they never reached the
	// collector even before the fix, while the plain value fields below did. The stdout sink is the
	// reverse — encoding/json follows pointers, so it leaked the whole graph. Neither sink's test
	// covers the other.
	for _, leaked := range []struct{ what, needle string }{
		{"the user's email", "alice.anderson@example.com"},
		{"their username", "alice"},
		{"their display name", "Alice Anderson"},
		{"their GitHub login", "alice-a"},
		{"their job title", "Staff Engineer"},
		{"their hobbies", "climbing"},
	} {
		if strings.Contains(out, leaked.needle) {
			t.Errorf("%s (%q) was shipped to the collector:\n%s", leaked.what, leaked.needle, out)
		}
	}

	// Volume, asserted. This attribute rides on every log line the request produces.
	const ceiling = 300
	if len(out) > ceiling {
		t.Errorf("exported record renders to %d bytes, want under %d:\n%s", len(out), ceiling, out)
	}
}
