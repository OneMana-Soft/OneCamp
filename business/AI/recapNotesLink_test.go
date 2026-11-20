package business

import (
	"testing"

	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
)

// The notes doc backfill (V-1209) reads back the link a recap writes: the
// two stay one format.
func TestTheBackfillReadsTheNotesLinkARecapWrites(t *testing.T) {
	id := "9b2f6a8e-1c4d-4e5f-8a9b-0c1d2e3f4a5b"
	if got := recordingDomain.RecapNotesDoc(recapToHTML("We shipped.", "", id, "")); got != id {
		t.Fatalf("got %q", got)
	}
	if got := recordingDomain.RecapNotesDoc(recapToHTML("We shipped.", "", "", "")); got != "" {
		t.Fatalf("no notes doc, got %q", got)
	}
}
