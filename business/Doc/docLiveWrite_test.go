package businness

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	followModels "github.com/akashc777/OneCamp/models/postgres/Follow"
	"github.com/akashc777/OneCamp/services/collab"
)

// seedsSeen is the Live Doc Writer, noting each seed's options and answering
// err.
type seedsSeen struct {
	collab.Writer
	opts []collab.Options
	err  error
}

func (s *seedsSeen) Seed(_ context.Context, _, _ string, o collab.Options) (collab.Result, error) {
	s.opts = append(s.opts, o)
	if s.err != nil {
		return collab.Result{}, s.err
	}
	return collab.Result{Persisted: true}, nil
}

func withSeeds(t *testing.T, err error) *seedsSeen {
	t.Helper()
	s := &seedsSeen{err: err}
	old := collab.Live
	collab.Live = s
	t.Cleanup(func() { collab.Live = old })
	return s
}

// A template's seed is loud, as every SeedBodyLive is (the demo's, an agent's
// new doc): making a doc from a template is a person's act, and the mentions
// in it are theirs to send. A copy's seed is quiet.
func TestATemplatesSeedIsLoudAndACopysQuiet(t *testing.T) {
	s := withSeeds(t, nil)
	by := collab.By{UserUUID: sam}
	if _, err := SeedBodyLive(context.Background(), d2, "<p>From the template.</p>", by); err != nil {
		t.Fatal(err)
	}
	if _, err := seedBodyWith(context.Background(), d3, "<p>Copied.</p>", collab.Options{By: by, Quiet: true}); err != nil {
		t.Fatal(err)
	}
	if len(s.opts) != 2 || s.opts[0].Quiet || !s.opts[1].Quiet || s.opts[0].By != by || s.opts[1].By != by {
		t.Fatalf("seeds %+v: want the template's loud and the copy's quiet, both Sam's", s.opts)
	}
}

// With the collaboration service down, a new doc's first body is written
// directly, and no save hook runs. A quiet seed's mentions and links are kept
// there as a quiet save keeps them, telling nobody, so the first edit after
// tells only whom that edit adds.
func TestAQuietSeedWrittenDirectlyKeepsWhoItMentions(t *testing.T) {
	withSeeds(t, collab.ErrUnavailable)
	ev := withEmitted(t)
	oldRead, oldWrite, oldS, oldR, oldW := readStoredBody, writeFirstBody, syncMentions, readDocLinks, writeDocLinks
	t.Cleanup(func() {
		readStoredBody, writeFirstBody, syncMentions, readDocLinks, writeDocLinks = oldRead, oldWrite, oldS, oldR, oldW
	})
	readStoredBody = func(context.Context, string) (string, error) { return "", nil }
	written := map[string]string{}
	writeFirstBody = func(_ context.Context, docUUID, html string) error {
		written[docUUID] = html
		return nil
	}
	kept := map[string][]string{}
	var bulk []bool
	syncMentions = func(ctx context.Context, docUUID string, people []string) (followModels.MentionSync, error) {
		kept[docUUID] = people
		bulk = append(bulk, helpers.IsBulkImport(ctx))
		// The doc's first keeping: everyone it mentions is new.
		return followModels.MentionSync{Added: people, First: true}, nil
	}
	made := time.Now()
	readDocLinks = func(context.Context, string) (*dgraphStruct.DgraphDoc, error) {
		return &dgraphStruct.DgraphDoc{Uid: "0x7", CreatedAt: &made}, nil
	}
	var linked []string
	writeDocLinks = func(_ context.Context, _ *dgraphStruct.DgraphDoc, refs []string) (bool, error) {
		linked = refs
		return true, nil
	}

	const copyDoc = "99999999-9999-4999-8999-999999999999"
	body := `<p><span data-type="mention" data-id="` + maya + `@0x3">@Maya</span> owns <a href="/app/doc/` + d2 + `">the plan</a>.</p>`
	if _, err := seedBodyWith(context.Background(), copyDoc, body, collab.Options{By: collab.By{UserUUID: sam}, Quiet: true}); err != nil {
		t.Fatal(err)
	}
	if written[copyDoc] != body {
		t.Fatalf("the body written directly: %q", written[copyDoc])
	}
	if !slices.Equal(kept[copyDoc], []string{maya}) || !slices.Equal(linked, []string{d2}) {
		t.Fatalf("kept mentions %v and links %v, want [maya] and [d2]", kept[copyDoc], linked)
	}
	if !slices.Equal(bulk, []bool{true}) {
		t.Errorf("the mentions were kept from a bulk import's context: %v, want [true], as a quiet save's are", bulk)
	}
	if len(ev.events) != 0 {
		t.Fatalf("a quiet seed written directly told %+v", ev.events)
	}
}

// Only a page has a stored body to read (docs F1): a folder, a file or a link
// answers ErrNotAPage, so nothing that reads one takes it for a page.
func TestOnlyAPageHasAStoredBody(t *testing.T) {
	if body, err := storedBodyOf(&dgraphStruct.DgraphDoc{Body: "<p>Plans.</p>"}); err != nil || body != "<p>Plans.</p>" {
		t.Fatalf("a page's stored body: %q, %v", body, err)
	}
	for _, kind := range []string{NodeFolder, NodeLink, domain.NodeFile} {
		if body, err := storedBodyOf(&dgraphStruct.DgraphDoc{NodeKind: kind}); !errors.Is(err, ErrNotAPage) || body != "" {
			t.Errorf("a %s's stored body: %q, %v; want ErrNotAPage", kind, body, err)
		}
	}
	if _, err := storedBodyOf(nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("no doc's stored body: %v, want ErrNotFound", err)
	}
}

// A seed into what isn't a page writes nothing. The collaboration service
// opens pages only, and its refusal reads as the service being down, so the
// seed falls back to writing a new doc's first body directly: that fallback
// answers ErrNotAPage, and writes no body and keeps no mention.
func TestASeedIntoANonPageWritesNothing(t *testing.T) {
	withSeeds(t, collab.ErrUnavailable)
	ev := withEmitted(t)
	oldRead, oldWrite, oldS := readStoredBody, writeFirstBody, syncMentions
	t.Cleanup(func() { readStoredBody, writeFirstBody, syncMentions = oldRead, oldWrite, oldS })
	readStoredBody = func(context.Context, string) (string, error) { return "", ErrNotAPage }
	wrote, kept := 0, 0
	writeFirstBody = func(context.Context, string, string) error {
		wrote++
		return nil
	}
	syncMentions = func(context.Context, string, []string) (followModels.MentionSync, error) {
		kept++
		return followModels.MentionSync{}, nil
	}

	body := `<p><span data-type="mention" data-id="` + maya + `@0x3">@Maya</span> owns the plan.</p>`
	if _, err := seedBodyWith(context.Background(), d3, body, collab.Options{By: collab.By{UserUUID: sam}, Quiet: true}); !errors.Is(err, ErrNotAPage) {
		t.Fatalf("a seed into a folder answered %v, want ErrNotAPage", err)
	}
	if wrote != 0 || kept != 0 || len(ev.events) != 0 {
		t.Fatalf("a seed into a folder wrote %d bodies, kept mentions %d times, emitted %+v", wrote, kept, ev.events)
	}
}

// A copied body's mentions are kept before its seed (KeepCopiedMentions, as
// DuplicateDocs and Content IO's import call it): whoever it mentions was told
// where it was written, so the content is attributed, not the save (N1 gap
// 1). The seed's save then tells nobody of them even when it comes back loud,
// its quiet mark lost to a restart or past its hour, while a mention typed in
// the doc since is told.
func TestACopiedBodysMentionsKeptFirstTellNobodyFromALoudSave(t *testing.T) {
	ev := withEmitted(t)
	oldS, oldR, oldW := syncMentions, readDocLinks, writeDocLinks
	t.Cleanup(func() { syncMentions, readDocLinks, writeDocLinks = oldS, oldR, oldW })
	// Docs' mentions began to be kept before this doc was made (migration 344),
	// so its first keeping is an ordinary one.
	made := time.Now()
	kept := map[string][]string{}
	syncMentions = func(_ context.Context, docUUID string, people []string) (followModels.MentionSync, error) {
		was, had := kept[docUUID]
		var added []string
		for _, p := range people {
			if !slices.Contains(was, p) {
				added = append(added, p)
			}
		}
		kept[docUUID] = people
		return followModels.MentionSync{Added: added, First: !had, Since: made.Add(-time.Hour)}, nil
	}
	readDocLinks = func(context.Context, string) (*dgraphStruct.DgraphDoc, error) {
		return &dgraphStruct.DgraphDoc{Uid: "0x8", CreatedAt: &made}, nil
	}
	writeDocLinks = func(context.Context, *dgraphStruct.DgraphDoc, []string) (bool, error) { return true, nil }

	const copyDoc = "77777777-7777-4777-8777-777777777777"
	seed := `<p><span data-type="mention" data-id="` + maya + `@0x3">@Maya</span> owns the plan.</p>`
	KeepCopiedMentions(context.Background(), copyDoc, seed)
	if !slices.Equal(kept[copyDoc], []string{maya}) {
		t.Errorf("kept before the seed: %v, want Maya", kept[copyDoc])
	}

	// The seed's save comes back loud, holding a mention of Jo that Sam typed
	// meanwhile: Jo is told, and Maya, whom the copied body mentions, isn't.
	typed := seed + `<p><span data-type="mention" data-id="` + jo + `@0x4">@Jo</span>, please review.</p>`
	mentionsSaved(context.Background(), Change{DocUUID: copyDoc, Body: typed, Contributors: []string{sam}, SavedAt: time.Now()})
	var told [][]string
	for _, e := range ev.events {
		told = append(told, e.Subjects)
	}
	if len(told) != 1 || !slices.Equal(told[0], []string{jo}) {
		t.Fatalf("the loud save told %v; want Jo alone", told)
	}
}
