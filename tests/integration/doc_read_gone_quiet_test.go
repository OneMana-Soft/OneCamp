//go:build integration
// +build integration

package integration_test

// A read of a doc that is gone is an answer, not a failure, against a real
// Dgraph: the reads the demo's logs caught (15 Oct 2026) answer not found and
// log nothing, and a read that fails is still logged, naming the doc.
//
// On the demo, every page deleted for good a moment after its last edit left
// "models/GetDgraphDocInfoByUUID failed to get dgraph doc" at error, twice: the
// doc.updated webhook sweep read it as its minute closed (business/Follow
// flushUpdates, GetDocCommentParticipants), and the idle-version sweep read its
// body a minute later (business/Snapshot takeIdleVersions, GetSystemDocByUUID,
// which logged it twice more). Both skip a doc that is gone. So did every
// request for one: a link opened after its doc went answered 404, and the
// collaboration service's read of it too, each with the same error line.
//
// Run: go test -tags=integration ./tests/integration/ -run TestReadingADocThatIsGoneLogsNoError -v

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
)

// errorLines keeps the error lines logged while a test runs.
type errorLines struct {
	mu    sync.Mutex
	lines []string
}

func (h *errorLines) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelError }
func (h *errorLines) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lines = append(h.lines, r.Message)
	return nil
}
func (h *errorLines) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *errorLines) WithGroup(string) slog.Handler      { return h }

func (h *errorLines) take() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.lines
	h.lines = nil
	return out
}

func TestReadingADocThatIsGoneLogsNoError(t *testing.T) {
	// The logger records from before the harness starts, and is put back
	// after it has gone (cleanups run last first).
	logged := &errorLines{}
	was := helpers.Logger
	helpers.Logger = slog.New(logged)
	t.Cleanup(func() { helpers.Logger = was })

	ctx := context.Background()
	dg := integration.SetupDgraph(t)

	// One doc that never was, and one deleted for good: made, then removed
	// whole, as a purge removes it.
	never := uuid.NewString()
	purged := uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{{"uid": "_:d", "dgraph.type": "Doc", "doc_uuid": purged, "doc_title": "Scratch"}})
	if _, err := dgraphInit.DgraphClient.NewTxn().Mutate(ctx, &api.Mutation{DelNquads: []byte("<" + uids["d"] + "> * * .\n"), CommitNow: true}); err != nil {
		t.Fatal(err)
	}
	logged.take()

	reads := map[string]func(ctx context.Context, id string) (*dgraphStruct.DgraphDoc, error){
		// The doc.updated webhook sweep's read (business/Follow docPeople).
		"GetDocCommentParticipants": docDomain.GetDocCommentParticipants,
		// The idle-version sweep's and the collaboration service's.
		"business GetSystemDocByUUID": docBusiness.GetSystemDocByUUID,
		"GetCollabDocByUUID":          docDomain.GetCollabDocByUUID,
		"domain GetSystemDocByUUID":   docDomain.GetSystemDocByUUID,
		"GetDocPermissions":           docDomain.GetDocPermissions,
		// A person's reads, by their graph node.
		"GetDocByDocUUID": func(ctx context.Context, id string) (*dgraphStruct.DgraphDoc, error) {
			return docBusiness.GetDocByDocUUID(ctx, id, "0x1")
		},
		"GetBasicDgraphDocByUUID": func(ctx context.Context, id string) (*dgraphStruct.DgraphDoc, error) {
			return docBusiness.GetBasicDgraphDocByUUID(ctx, id, "0x1")
		},
		"GetDgraphDocByUUIDOnlyEditingInfo": func(ctx context.Context, id string) (*dgraphStruct.DgraphDoc, error) {
			return docBusiness.GetDgraphDocByUUIDOnlyEditingInfo(ctx, id, "0x1")
		},
		"GetDocCommentList": func(ctx context.Context, id string) (*dgraphStruct.DgraphDoc, error) {
			return docBusiness.GetDocCommentList(ctx, id, "0x1")
		},
		"GetDocWithChildren": func(ctx context.Context, id string) (*dgraphStruct.DgraphDoc, error) {
			return docDomain.GetDocWithChildren(ctx, id, "0x1", 10)
		},
		"GetDocForTrashPage": func(ctx context.Context, id string) (*dgraphStruct.DgraphDoc, error) {
			return docDomain.GetDocForTrashPage(ctx, id, "0x1")
		},
	}
	for name, read := range reads {
		for _, id := range []string{never, purged} {
			if doc, err := read(ctx, id); !errors.Is(err, helpers.ErrNotFound) || doc != nil {
				t.Errorf("%s of a doc that is gone: %+v, %v (want not found)", name, doc, err)
			}
		}
	}
	if lines := logged.take(); len(lines) > 0 {
		t.Fatalf("reads of docs that are gone logged %d error lines, the first %q", len(lines), lines[0])
	}

	t.Run("a read that fails is still logged, naming the doc", func(t *testing.T) {
		dg.Break(t)
		if _, err := docBusiness.GetSystemDocByUUID(ctx, purged); err == nil || errors.Is(err, helpers.ErrNotFound) {
			t.Fatalf("a read the graph couldn't answer: %v", err)
		}
		found := false
		for _, l := range logged.take() {
			found = found || strings.Contains(l, "domain/GetCollabDocByUUID doc "+purged)
		}
		if !found {
			t.Fatal("the failed read wasn't logged with its doc")
		}
	})
}
