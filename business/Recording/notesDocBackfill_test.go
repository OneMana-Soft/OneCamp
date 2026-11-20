package business

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Recording"
)

func docN(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }

func recapLinking(doc string) string {
	return `<p>Recap</p><p><a href="/app/doc/` + doc + `">📄 Open the meeting notes</a></p>`
}

type backfillWorld struct {
	pages []string // the after of each page read
	bots  []string // the bot each page's recaps were read by
	links []map[string]string
	ended map[string]time.Time // each link's end, by call
	calls []domain.UnlinkedCall
	fail  int // the page (1-based) whose read fails, 0 for none
}

const backfillBot = "0xb07"

func newBackfillWorld(t *testing.T, calls []domain.UnlinkedCall) *backfillWorld {
	t.Helper()
	w := &backfillWorld{calls: calls, ended: map[string]time.Time{}}
	prevRead, prevLink, prevBot := unlinkedCalls, linkNotesDocs, automationBot
	t.Cleanup(func() { unlinkedCalls, linkNotesDocs, automationBot = prevRead, prevLink, prevBot })
	automationBot = func(context.Context) string { return backfillBot }
	unlinkedCalls = func(_ context.Context, bot, after string, first int) ([]domain.UnlinkedCall, error) {
		w.pages = append(w.pages, after)
		w.bots = append(w.bots, bot)
		if len(w.pages) == w.fail {
			return nil, errors.New("dgraph went away")
		}
		var out []domain.UnlinkedCall
		for _, c := range w.calls {
			if c.Uid > after && len(out) < first {
				out = append(out, c)
			}
		}
		return out, nil
	}
	linkNotesDocs = func(_ context.Context, links []domain.NotesLink) (int, error) {
		page := map[string]string{}
		for _, l := range links {
			page[l.Call] = l.Doc
			w.ended[l.Call] = l.Ended
		}
		w.links = append(w.links, page)
		return len(links), nil
	}
	return w
}

func TestTheBackfillLinksEachCallToTheOneDocItsRecapNamed(t *testing.T) {
	var calls []domain.UnlinkedCall
	for i := 0; i < 101; i++ {
		calls = append(calls, domain.UnlinkedCall{Uid: fmt.Sprintf("0x%04x", i+1), Recaps: []string{recapLinking(docN(i))}})
	}
	// No recap, a recap naming no doc, and two recaps naming different docs:
	// none is linked (a wrong link is worse than none).
	calls = append(calls,
		domain.UnlinkedCall{Uid: "0x0200"},
		domain.UnlinkedCall{Uid: "0x0201", Recaps: []string{"<p>Recap only</p>"}},
		domain.UnlinkedCall{Uid: "0x0202", Recaps: []string{recapLinking(docN(997)), recapLinking(docN(998))}},
		domain.UnlinkedCall{Uid: "0x0203", Recaps: []string{recapLinking(docN(999)), recapLinking(docN(999))}},
	)
	w := newBackfillWorld(t, calls)
	got, err := backfillNotesDocs(context.Background())
	if err != nil || got != (NotesDocBackfillCount{Scanned: 105, Linked: 102, NoDoc: 2, ManyDocs: 1}) {
		t.Fatalf("%+v %v", got, err)
	}
	if got.String() != "scanned 105 calls, linked 102, skipped 2 naming no notes doc and 1 naming more than one" {
		t.Fatalf("logged %q", got)
	}
	// A page of 100 per read, each page after the last call of the one before.
	if !reflect.DeepEqual(w.pages, []string{"", "0x0064"}) {
		t.Fatalf("pages %v", w.pages)
	}
	if len(w.links) != 2 || len(w.links[0]) != 100 || w.links[0]["0x0001"] != docN(0) {
		t.Fatalf("%d writes, first page %d links", len(w.links), len(w.links[0]))
	}
	last := w.links[1]
	if !reflect.DeepEqual(last, map[string]string{"0x0065": docN(100), "0x0203": docN(999)}) {
		t.Fatalf("last page %v", last)
	}
}

// MAIN 18 Nov: a call whose window holds zero or more than one candidate doc
// is linked to NOTHING, and the run counts each kind apart. It never guesses:
// not the first doc, not the latest, not the one named most.
func TestACallNamingNoDocOrTwoDocsIsLinkedToNothingAndCountedApart(t *testing.T) {
	for _, c := range []struct {
		name   string
		recaps []string
		want   NotesDocBackfillCount
	}{
		{"no recap", nil, NotesDocBackfillCount{Scanned: 1, NoDoc: 1}},
		{"a recap naming no doc", []string{"<p>Recap only</p>"}, NotesDocBackfillCount{Scanned: 1, NoDoc: 1}},
		{"two docs", []string{recapLinking(docN(1)), recapLinking(docN(2))}, NotesDocBackfillCount{Scanned: 1, ManyDocs: 1}},
		{"two docs, one named twice", []string{recapLinking(docN(1)), recapLinking(docN(2)), recapLinking(docN(1))},
			NotesDocBackfillCount{Scanned: 1, ManyDocs: 1}},
		{"one doc named twice", []string{recapLinking(docN(3)), recapLinking(docN(3))}, NotesDocBackfillCount{Scanned: 1, Linked: 1}},
	} {
		w := newBackfillWorld(t, []domain.UnlinkedCall{{Uid: "0x1", Recaps: c.recaps}})
		got, err := backfillNotesDocs(context.Background())
		if err != nil || got != c.want {
			t.Fatalf("%s: %+v %v", c.name, got, err)
		}
		if c.want.Linked == 0 && len(w.links) != 0 {
			t.Fatalf("%s: wrote %v", c.name, w.links)
		}
	}
}

// F1 (CR-429, MAIN 18 Nov): the recaps are read only by the AI bot's stored
// uid, on every page, and each link carries its call's end so the write can
// check the doc against the call's window. With no bot resolved nothing is
// read and the run is not done, so a later start tries again.
func TestTheBackfillReadsOnlyTheAIBotsRecapsAndEachLinkCarriesItsCallsEnd(t *testing.T) {
	ended := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	var calls []domain.UnlinkedCall
	for i := 0; i < 101; i++ {
		calls = append(calls, domain.UnlinkedCall{Uid: fmt.Sprintf("0x%04x", i+1), Ended: ended.Add(time.Duration(i) * time.Minute),
			Recaps: []string{recapLinking(docN(i))}})
	}
	w := newBackfillWorld(t, calls)
	if _, err := backfillNotesDocs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w.bots, []string{backfillBot, backfillBot}) {
		t.Fatalf("pages read by %v, want the bot's uid on each", w.bots)
	}
	if !w.ended["0x0065"].Equal(ended.Add(100*time.Minute)) || len(w.ended) != 101 {
		t.Fatalf("0x0065's link ended %v (%d links)", w.ended["0x0065"], len(w.ended))
	}

	w = newBackfillWorld(t, calls)
	automationBot = func(context.Context) string { return "" }
	if got, err := backfillNotesDocs(context.Background()); err == nil || len(w.pages) != 0 || got.Linked != 0 {
		t.Fatalf("with no bot: %+v %v, %d pages read", got, err, len(w.pages))
	}
}

// A page that can't be read ends the run with its error, so repairBusiness.Once
// records nothing and the next try walks again: the run is done only after its
// last page.
func TestABackfillThatFailsPartWayIsNotDone(t *testing.T) {
	var calls []domain.UnlinkedCall
	for i := 0; i < 150; i++ {
		calls = append(calls, domain.UnlinkedCall{Uid: fmt.Sprintf("0x%04x", i+1), Recaps: []string{recapLinking(docN(i))}})
	}
	w := newBackfillWorld(t, calls)
	w.fail = 2
	got, err := backfillNotesDocs(context.Background())
	if err == nil || got.Linked != 100 || len(w.pages) != 2 {
		t.Fatalf("%+v %v after %d pages", got, err, len(w.pages))
	}
	if NotesDocBackfill.Key != notesDocBackfillKey || NotesDocBackfill.Repair == nil || NotesDocBackfill.Tries < 1 {
		t.Fatalf("the backfill is not a repair run once: %+v", NotesDocBackfill)
	}
}

func TestARecapsNotesDocIsTheLinkTheRecapWrote(t *testing.T) {
	id := "9b2f6a8e-1c4d-4e5f-8a9b-0c1d2e3f4a5b"
	if got := domain.RecapNotesDoc(recapLinking(id)); got != id {
		t.Fatalf("got %q", got)
	}
	for _, html := range []string{"", "<p>Recap</p>", `<a href="/app/doc/` + id + `">a doc someone linked</a>`} {
		if got := domain.RecapNotesDoc(html); got != "" {
			t.Fatalf("%q -> %q", html, got)
		}
	}
}
