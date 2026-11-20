package business

// The notes doc backfill (V-1209 B): a call's notes doc made before v2.77.0
// is tied to its conversation by the recap it posted there, so a handover of
// a departed owner's docs finds it (business/Departed). It runs to the end
// once per install (repairBusiness.Once: its system_configs key records that
// it has, only after the last page), in the background after the server has
// started, a page of 100 calls at a time; each page is two reads and one
// write, and a call that already names its notes doc is never written.

import (
	"context"
	"errors"
	"fmt"
	"slices"

	repairBusiness "github.com/akashc777/OneCamp/business/Repair"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	domain "github.com/akashc777/OneCamp/domain/Recording"
)

const (
	notesDocBackfillPage = 100
	// notesDocBackfillKey records in system_configs that the backfill ran to
	// the end on this install.
	notesDocBackfillKey   = "recording_notes_doc_backfill_v1"
	notesDocBackfillTries = 8
)

// Seams for tests.
var (
	unlinkedCalls = domain.UnlinkedNotesCalls
	linkNotesDocs = domain.LinkNotesDocs
	automationBot = func(ctx context.Context) string {
		if bot := userBusiness.GetAutomationBot(ctx); bot != nil {
			return bot.DgraphUID
		}
		return ""
	}
)

// NotesDocBackfillCount is what a run did: the calls it read, the ones it
// linked, and the ones it skipped because their recaps named no notes doc
// (NoDoc) or more than one (ManyDocs). It never guesses (MAIN, 18 Nov).
type NotesDocBackfillCount struct {
	Scanned, Linked, NoDoc, ManyDocs int
}

func (c NotesDocBackfillCount) String() string {
	return fmt.Sprintf("scanned %d calls, linked %d, skipped %d naming no notes doc and %d naming more than one",
		c.Scanned, c.Linked, c.NoDoc, c.ManyDocs)
}

// NotesDocBackfill links each unlinked call to the one notes doc its recap
// named, once per install. Start it after the server has started.
var NotesDocBackfill = repairBusiness.Once[NotesDocBackfillCount]{
	Key: notesDocBackfillKey, Name: "recording.notes-doc-backfill", Tries: notesDocBackfillTries, Repair: backfillNotesDocs,
}

// backfillNotesDocs walks every unlinked call, a page at a time, and links
// each to the one notes doc its recaps name. An error stops the walk, so the
// run is not recorded and the next try walks again (a linked call is no
// longer read).
func backfillNotesDocs(ctx context.Context) (c NotesDocBackfillCount, err error) {
	// The recaps are the AI bot's own posts, by its stored uid (CR-429 F1).
	// Without it nothing is read, and the run is not done: a later start
	// tries again.
	bot := automationBot(ctx)
	if bot == "" {
		return c, errors.New("business/backfillNotesDocs: the AI bot isn't resolved")
	}
	after := ""
	for {
		page, err := unlinkedCalls(ctx, bot, after, notesDocBackfillPage)
		if err != nil {
			return c, err
		}
		c.Scanned += len(page)
		var links []domain.NotesLink
		for _, call := range page {
			after = call.Uid
			switch docs := notesDocsNamed(call.Recaps); len(docs) {
			case 0:
				c.NoDoc++
			case 1:
				links = append(links, domain.NotesLink{Call: call.Uid, Ended: call.Ended, Doc: docs[0]})
			default:
				c.ManyDocs++
			}
		}
		if len(links) > 0 {
			n, err := linkNotesDocs(ctx, links)
			if err != nil {
				return c, err
			}
			c.Linked += n
		}
		if len(page) < notesDocBackfillPage {
			return c, nil
		}
	}
}

// notesDocsNamed is the distinct notes docs a call's recaps name, in the
// order they are read. Only a call naming exactly one is linked: none or more
// than one is skipped, as a wrong link is worse than none. Pure.
func notesDocsNamed(recaps []string) []string {
	var docs []string
	for _, html := range recaps {
		if d := domain.RecapNotesDoc(html); d != "" && !slices.Contains(docs, d) {
			docs = append(docs, d)
		}
	}
	return docs
}
