package business

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
)

func swap[T any](t *testing.T, p *T, v T) {
	t.Helper()
	saved := *p
	t.Cleanup(func() { *p = saved })
	*p = v
}

// M10: an index write that fails logs ids and counts, never the line's
// words, though the index's own error echoes them; its error and the
// request's span carry none either.
func TestAFailedIndexWriteNeverLogsTheLine(t *testing.T) {
	const words = "the acquisition closes on friday"
	swap(t, &readEntry, func(_ context.Context, w Written) (*openSearchStruct.OpenSearchTranscript, error) {
		return &openSearchStruct.OpenSearchTranscript{Uuid: w.LineUID, Text: words, RecordingID: w.Key, ConvID: "ch-1"}, nil
	})
	swap(t, &writeEntries, func(context.Context, []*openSearchStruct.OpenSearchTranscript) error {
		return errors.New(`mapper_parsing_exception: failed to parse field [transcript_offset_ms]. Preview of field's value: '` + words + `'`)
	})
	woke := 0
	swap(t, &wakeBackfill, func() { woke++ })
	var logs bytes.Buffer
	swap(t, &helpers.Logger, slog.New(slog.NewTextHandler(&logs, nil)))
	spans := tracetest.NewSpanRecorder()
	ctx, span := trace.NewTracerProvider(trace.WithSpanProcessor(spans)).Tracer("t").Start(context.Background(), "store line")

	err := IndexLine(ctx, Written{Key: "EG_1", LineUID: "0x9", SessionKey: "call-RM_1", Room: "ch-1"})
	span.End()
	if err == nil {
		t.Fatal("a failed write answered no error")
	}
	if woke != 1 {
		t.Errorf("the failure woke the repair %d times, want 1", woke)
	}
	for _, w := range strings.Fields(words) {
		if len(w) > 4 && (strings.Contains(logs.String(), w) || strings.Contains(err.Error(), w)) {
			t.Errorf("the line's word %q is in the log %q or the error %q", w, logs.String(), err)
		}
	}
	if !strings.Contains(logs.String(), "0x9") {
		t.Errorf("the log %q doesn't name the line's id", logs.String())
	}
	for _, s := range spans.Ended() {
		for _, a := range s.Attributes() {
			if strings.Contains(a.Value.String(), "acquisition") {
				t.Errorf("span %s carries %s=%s", s.Name(), a.Key, a.Value.String())
			}
		}
		for _, e := range s.Events() {
			for _, a := range e.Attributes {
				if strings.Contains(a.Value.String(), "acquisition") {
					t.Errorf("span %s's event %s carries the line", s.Name(), e.Name)
				}
			}
		}
	}
}

// M9: the backup takes the transcripts index: the server makes it, and the
// snapshot's index pattern takes every index the server makes. Its words are
// the line's own: the line and its speaker's name are its only text.
func TestTheBackupTakesTheTranscripts(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "other-services", "opensearch", "snapshot.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^content='([^']+)'`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("snapshot.sh names no content pattern")
	}
	if !snapshotTakes(m[1], openSearchStruct.TRANSCRIPT_INDEX) {
		t.Errorf("the snapshot's pattern %q leaves out %q", m[1], openSearchStruct.TRANSCRIPT_INDEX)
	}
	if snapshotTakes(m[1], ".hidden") || !snapshotTakes("*", "posts") {
		t.Error("the pattern reader is wrong")
	}
	made := indicesMade(t)
	body, ok := made[openSearchStruct.TRANSCRIPT_INDEX]
	if !ok {
		t.Fatalf("the server makes no %s index", openSearchStruct.TRANSCRIPT_INDEX)
	}
	for index := range made {
		if !snapshotTakes(m[1], index) {
			t.Errorf("the snapshot's pattern %q leaves out %q, an index the server makes", m[1], index)
		}
	}
	var mapping struct {
		Mappings struct {
			Properties map[string]struct {
				Type string `json:"type"`
			} `json:"properties"`
		} `json:"mappings"`
	}
	if err := json.Unmarshal([]byte(body), &mapping); err != nil {
		t.Fatalf("the transcripts mapping: %v", err)
	}
	props := mapping.Mappings.Properties
	for f, p := range props {
		if p.Type == "text" && f != "transcript_text" && f != "transcript_by_user_full_name" {
			t.Errorf("transcripts hold %s as words, wider than the line", f)
		}
	}
	if props["transcript_text"].Type != "text" || props["transcript_offset_ms"].Type != "long" {
		t.Errorf("the mapping holds the line as %q and its moment as %q", props["transcript_text"].Type, props["transcript_offset_ms"].Type)
	}
}

// indicesMade are the indices ConnectOpenSearch makes, with their bodies,
// read from its source.
func indicesMade(t *testing.T) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "..", "initializers", "opensearchInit", "connectOpensearch.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	made := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		a, ok := n.(*ast.AssignStmt)
		if !ok || len(a.Lhs) != 1 || len(a.Rhs) != 1 {
			return true
		}
		if id, ok := a.Lhs[0].(*ast.Ident); !ok || id.Name != "indicesToCreate" {
			return true
		}
		lit, ok := a.Rhs[0].(*ast.CompositeLit)
		if !ok {
			t.Fatal("indicesToCreate isn't a literal")
		}
		for _, e := range lit.Elts {
			kv, ok := e.(*ast.KeyValueExpr)
			if !ok {
				t.Fatalf("an index made isn't a name and a body: %T", e)
			}
			name, ok := literal(kv.Key)
			if !ok {
				t.Fatalf("an index made isn't named by a literal: %T", kv.Key)
			}
			made[name], _ = literal(kv.Value)
		}
		return false
	})
	if len(made) == 0 {
		t.Fatal("connectOpensearch.go makes no indices")
	}
	return made
}

// literal is a string literal's text, or a sum of them; false when a part
// isn't one (its text is then left out).
func literal(e ast.Expr) (string, bool) {
	switch e := e.(type) {
	case *ast.BasicLit:
		s, err := strconv.Unquote(e.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		l, lok := literal(e.X)
		r, rok := literal(e.Y)
		return l + r, lok && rok && e.Op == token.ADD
	}
	return "", false
}

// snapshotTakes is whether OpenSearch's comma pattern (a leading "-" leaves
// out what follows, "*" matches anything) takes index.
func snapshotTakes(pattern, index string) bool {
	in := false
	for _, p := range strings.Split(pattern, ",") {
		out := strings.HasPrefix(p, "-")
		re := "^" + strings.ReplaceAll(regexp.QuoteMeta(strings.TrimPrefix(p, "-")), `\*`, ".*") + "$"
		if regexp.MustCompile(re).MatchString(index) {
			in = !out
		}
	}
	return in
}

// MB1: a deleted home's lines are written deleted, as its delete marks them,
// so a write that races a delete never makes them count for members; a live
// home's (a restore writes the zero time) are written live.
func TestADeletedRecordingsLinesAreWrittenDeleted(t *testing.T) {
	gone := time.Unix(1_700_000_000, 0)
	home := &dgraphStruct.DgraphRecording{EgressId: "EG_1", Channel: &dgraphStruct.DgraphChannel{Uuid: "ch-1"}, DeletedAt: &gone,
		Transcript: []*dgraphStruct.DgraphTranscript{{Uid: "0x1", Text: "the launch budget"}, {Uid: "0x2", Text: "is set"}}}
	entries := entriesOf(home, Written{Key: "EG_1"})
	if len(entries) != 2 {
		t.Fatalf("%d entries; want one per line", len(entries))
	}
	for _, e := range entries {
		if e.DeletedAt == nil || *e.DeletedAt != gone.Unix() || e.DeletedBy == nil || *e.DeletedBy != recordingDomain.TranscriptCascade {
			t.Errorf("line %s of a deleted recording is written with deleted_date %v by %v", e.Uuid, e.DeletedAt, e.DeletedBy)
		}
	}
	restored := time.Time{}
	home.DeletedAt = &restored
	for _, e := range entriesOf(home, Written{Key: "EG_1"}) {
		if e.DeletedAt != nil || e.DeletedBy != nil {
			t.Errorf("line %s of a restored recording is written deleted", e.Uuid)
		}
	}
}

// MB2: a pass records the backfill done only while its state is still the
// one the pass started from. A failed write reopens it with a new value, and
// a reopen that lands at any moment of the pass, even just before the done
// mark, keeps it reopened for the next pass.
func TestABackfillReopenedDuringItsPassIsNotRecordedDone(t *testing.T) {
	for _, c := range []struct {
		name   string
		reopen string // when a failed write reopens it: "", "write" or "mark"
		want   string
	}{
		{"a pass nothing reopened", "", "done"},
		{"a write failed during the pass", "write", "pending 2"},
		{"a write failed just before the done mark", "mark", "pending 2"},
	} {
		state := "pending 1"
		swap(t, &backfillState, func() (string, error) { return state, nil })
		swap(t, &markBackfill, func(value string) error { state = value; return nil })
		// The store's compare-and-set, as configModel.Swap writes it.
		swap(t, &markBackfillDone, func(from string) (bool, error) {
			if c.reopen == "mark" {
				state = "pending 2"
			}
			if state != from {
				return false, nil
			}
			state = "done"
			return true, nil
		})
		swap(t, &homesPage, func(context.Context, string, int) ([]*dgraphStruct.DgraphRecording, error) {
			return []*dgraphStruct.DgraphRecording{{EgressId: "EG_1", Channel: &dgraphStruct.DgraphChannel{Uuid: "ch-1"},
				Transcript: []*dgraphStruct.DgraphTranscript{{Uid: "0x1", Text: "the launch budget"}}}}, nil
		})
		swap(t, &writeEntries, func(context.Context, []*openSearchStruct.OpenSearchTranscript) error {
			if c.reopen == "write" {
				state = "pending 2"
			}
			return nil
		})
		if err := Backfill(context.Background()); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if state != c.want {
			t.Errorf("%s: the backfill is %q; want %q", c.name, state, c.want)
		}
	}
}
