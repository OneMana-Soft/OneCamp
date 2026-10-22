package business

import (
	"context"
	"errors"
	"testing"
	"time"

	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	pinBusiness "github.com/akashc777/OneCamp/business/Pin"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	"github.com/google/uuid"
)

// A channel's pins come across as the person who pinned each, at the time
// they did, in one write after three reads of the id map; a pin whose
// message, pinner or channel wasn't brought across is skipped, and the
// warnings are one write too. Never is a pin made as someone else.
func TestAChannelsPinsComeAcrossAsTheirPinners(t *testing.T) {
	launch, quiet := uuid.New(), uuid.New()
	ada, msg1, msg2 := uuid.New(), uuid.New(), uuid.New()
	mapped := map[string]uuid.UUID{
		importModels.EntityChannel + ":C1":       launch,
		importModels.EntityChannel + ":G1":       quiet,
		importModels.EntityMessage + ":C1/100.1": msg1, // known by its channel and ts
		importModels.EntityMessage + ":C1/100.2": msg2,
		importModels.EntityUser + ":U1":          ada,
	}
	wasLookup, wasPins, wasNotes := lookupMappedMany, importPins, notePins
	t.Cleanup(func() { lookupMappedMany, importPins, notePins = wasLookup, wasPins, wasNotes })
	reads := 0
	lookupMappedMany = func(_ context.Context, _ uuid.UUID, entity string, ids []string) (map[string]uuid.UUID, error) {
		reads++
		out := map[string]uuid.UUID{}
		for _, id := range ids {
			if v, ok := mapped[entity+":"+id]; ok {
				out[id] = v
			}
		}
		return out, nil
	}
	var written [][]pinBusiness.ImportedPin
	importPins = func(_ context.Context, pins []pinBusiness.ImportedPin) (map[int]error, error) {
		written = append(written, pins)
		refused := map[int]error{}
		for i, p := range pins {
			if p.Message == msg2.String() {
				refused[i] = errors.New("That message was deleted.")
			}
		}
		return refused, nil
	}
	var noted [][]importModels.ImportError
	notePins = func(_ context.Context, _ uuid.UUID, _ *uuid.UUID, entries []importModels.ImportError) {
		noted = append(noted, entries)
	}

	applied, skipped := applyPins(context.Background(), uuid.New(), uuid.New(), &ParsedExport{
		Channels: []SlackChannel{
			{ID: "C1", Name: "launch", Pins: []SlackPin{
				{ID: "100.1", Created: 1759309200, User: "U1"}, // pinned
				{ID: "100.9", Created: 1759309200, User: "U1"}, // its message wasn't brought
				{ID: "100.1", Created: 1759309200, User: "U9"}, // its pinner wasn't
				{ID: "100.2", Created: 1759309200, User: "U1"}, // Pin refuses it
			}},
			{ID: "C9", Name: "gone", Pins: []SlackPin{{ID: "100.1", User: "U1"}}}, // its channel wasn't
		},
		Groups: []SlackChannel{{ID: "G1", Name: "quiet"}},
	})
	if applied != 1 || skipped != 4 {
		t.Fatalf("applied %d, skipped %d; want 1 and 4", applied, skipped)
	}
	if reads != 3 || len(written) != 1 || len(noted) != 1 {
		t.Fatalf("%d reads, %d writes of pins, %d of warnings: want 3, 1 and 1", reads, len(written), len(noted))
	}
	want := pinBusiness.ImportedPin{Ref: conversationBusiness.Ref{Kind: "channel", ID: launch.String()}, Message: msg1.String(), By: ada, At: time.Unix(1759309200, 0).UTC()}
	if len(written[0]) != 2 || written[0][0] != want {
		t.Fatalf("pinned %+v, want %+v first", written[0], want)
	}
	wantNoted := []string{"PIN_MESSAGE_NOT_IMPORTED 100.9", "PIN_PINNER_NOT_IMPORTED 100.1", "PIN_IMPORT_FAILED 100.2"}
	if len(noted[0]) != len(wantNoted) {
		t.Fatalf("noted %+v, want %v", noted[0], wantNoted)
	}
	for i, n := range noted[0] {
		if got := n.Code + " " + n.SourceID; got != wantNoted[i] || n.Severity != importModels.SeverityWarning {
			t.Errorf("noted %q (%s), want %q as a warning", got, n.Severity, wantNoted[i])
		}
	}

	// A read that fails pins nothing and says so once.
	lookupMappedMany = func(context.Context, uuid.UUID, string, []string) (map[string]uuid.UUID, error) {
		return nil, errors.New("the database didn't answer")
	}
	noted, written = nil, nil
	if applied, skipped := applyPins(context.Background(), uuid.New(), uuid.New(), &ParsedExport{
		Channels: []SlackChannel{{ID: "C1", Pins: []SlackPin{{ID: "100.1", User: "U1"}}}},
	}); applied != 0 || skipped != 1 || len(written) != 0 || len(noted) != 1 || noted[0][0].Code != "PIN_IMPORT_FAILED" {
		t.Errorf("a failed read: applied %d, skipped %d, written %v, noted %+v", applied, skipped, written, noted)
	}
}
