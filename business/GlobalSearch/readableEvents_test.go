package business

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	querySpec "github.com/akashc777/OneCamp/business/QuerySpec"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	opensearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
	"github.com/google/uuid"
)

// A unit test has no calendar store: whoever searches sees no calendars
// beyond their own events and invitations, unless a test says otherwise.
//
// Nor does it repair search entries: a search's repairs run where it was
// made and write nothing, unless a test records them. One left running in
// the background outlives its test and is made by whatever repairEntry a
// later test has put in its place (TestNoRepairOutlivesTheSearchThatAskedForIt).
func TestMain(m *testing.M) {
	searchCalendars = func(context.Context, accessBusiness.Subject) ([]string, []string, error) { return nil, nil, nil }
	goRepairs = func(repair func()) { repair() }
	repairEntry = func(kind, id string) {}
	os.Exit(m.Run())
}

func eventHit(id string) *openSearchStruct.GlobalSearchOpenSearchResp {
	return &openSearchStruct.GlobalSearchOpenSearchResp{Type: openSearchStruct.EVENT_TYPE, Event: &openSearchStruct.OpenSearchEvent{Uuid: id, EventTitle: "Roadmap review"}}
}

// An event's entry says who may find it, and the calendar's one rule says
// who may open it now: every event on a page is asked about, once for the
// page, as the person searching (an agent's bounds included), and kept only
// if they may open it. Anything else on the page is left as it is.
func TestSearchKeepsOnlyTheEventsThePersonCanOpenNow(t *testing.T) {
	open, private, trashed := uuid.NewString(), uuid.NewString(), uuid.NewString()
	who := accessBusiness.Subject{UserUUID: "U", UserUID: "0x1", AgentUUID: "A"}
	var asked [][]string
	var askedAs []accessBusiness.Subject
	swap(t, &readableEvents, func(_ context.Context, as accessBusiness.Subject, ids []string) (map[string]bool, error) {
		asked, askedAs = append(asked, ids), append(askedAs, as)
		return map[string]bool{open: true}, nil
	})
	hits := []*openSearchStruct.GlobalSearchOpenSearchResp{eventHit(open), eventHit(private), task(liveTask), eventHit(strings.ToUpper(trashed)), eventHit(open)}
	got := keepReadableEventHits(context.Background(), who, hits)
	if want := []*openSearchStruct.GlobalSearchOpenSearchResp{hits[0], hits[2], hits[4]}; !reflect.DeepEqual(got, want) {
		t.Errorf("kept %d hits, want the open event twice and the task", len(got))
	}
	if len(asked) != 1 || !slices.Equal(asked[0], []string{open, private, trashed}) || askedAs[0] != who {
		t.Errorf("asked %v as %+v; want once, about each event once, as the person searching", asked, askedAs)
	}

	// A page with no events asks nothing.
	asked = nil
	if got := keepReadableEventHits(context.Background(), who, []*openSearchStruct.GlobalSearchOpenSearchResp{task(liveTask)}); len(got) != 1 || asked != nil {
		t.Errorf("a page of no events: kept %d, asked %v", len(got), asked)
	}
}

// When the check can't answer, no event is shown: a result lost, never an
// event shown to someone who may not open it.
func TestAnEventCheckThatFailsLeavesEveryEventOut(t *testing.T) {
	swap(t, &readableEvents, func(context.Context, accessBusiness.Subject, []string) (map[string]bool, error) {
		return nil, errors.New("the store is down")
	})
	hits := []*openSearchStruct.GlobalSearchOpenSearchResp{eventHit(uuid.NewString()), task(liveTask)}
	if got := keepReadableEventHits(context.Background(), accessBusiness.Subject{UserUUID: "U"}, hits); len(got) != 1 || got[0] != hits[1] {
		t.Errorf("kept %d hits, want the task alone", len(got))
	}
}

// eventsClause is the events' access clause a search sent, and the
// indices it asked.
func eventsClause(t *testing.T, raw []byte) string {
	t.Helper()
	var body struct {
		Query struct {
			Bool struct {
				Should []struct {
					Bool struct {
						Filter []json.RawMessage `json:"filter"`
					} `json:"bool"`
				} `json:"should"`
			} `json:"bool"`
		} `json:"query"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	for _, s := range body.Query.Bool.Should {
		if len(s.Bool.Filter) == 3 && strings.Contains(string(s.Bool.Filter[0]), openSearchStruct.EVENT_INDEX) {
			return string(s.Bool.Filter[2])
		}
	}
	return ""
}

// The calendars someone finds events on are read once a search looks at
// events, and only then: a search of docs alone never reads them. The
// clause then finds their own events and invitations, every event on the
// calendars they may change, and those that aren't private on the ones they
// only see. A read that fails finds their own events and invitations alone,
// and the search still answers.
func TestSearchReadsTheCalendarsOnlyWhenItLooksAtEvents(t *testing.T) {
	allReady(t)
	withChecks(t, nil)
	who := accessBusiness.Subject{UserUUID: "U", UserUID: "0x1"}
	var reads []accessBusiness.Subject
	failing := false
	swap(t, &searchCalendars, func(_ context.Context, as accessBusiness.Subject) ([]string, []string, error) {
		reads = append(reads, as)
		if failing {
			return nil, nil, errors.New("the store is down")
		}
		return []string{"K1"}, []string{"K2"}, nil
	})
	var sent []byte
	var asked []string
	swap(t, &searchIndex, func(_ context.Context, indices []string, raw []byte) (opensearchModels.SearchAnswer, error) {
		sent, asked = raw, indices
		return opensearchModels.SearchAnswer{}, nil
	})
	reach := Reach{Who: who, UserUUID: "U"}

	docs := querySpec.Spec{Scope: querySpec.Scope{Kind: querySpec.ScopeSearch, Kinds: []string{KindDoc}}, Filter: launch().Filter}
	if _, err := Search(context.Background(), reach, docs, Page{}); err != nil {
		t.Fatal(err)
	}
	if len(reads) != 0 || slices.Contains(asked, openSearchStruct.EVENT_INDEX) {
		t.Errorf("a search of docs read the calendars %d times and asked %v", len(reads), asked)
	}

	if _, err := Search(context.Background(), reach, launch(), Page{}); err != nil {
		t.Fatal(err)
	}
	want := `{"bool":{"minimum_should_match":1,"should":[{"term":{"event_people":"U"}},{"terms":{"event_calendar_id":["K1"]}},` +
		`{"bool":{"filter":[{"terms":{"event_calendar_id":["K2"]}}],"must_not":[{"term":{"event_private":true}}]}}]}}`
	if got := eventsClause(t, sent); got != want {
		t.Errorf("the events' clause\n  got  %s\n  want %s", got, want)
	}
	if len(reads) != 1 || reads[0] != who {
		t.Errorf("read the calendars %d times as %+v, want once as the person searching", len(reads), reads)
	}

	failing = true
	if _, err := Search(context.Background(), reach, launch(), Page{}); err != nil {
		t.Fatalf("a search whose calendars couldn't be read failed: %v", err)
	}
	want = `{"bool":{"minimum_should_match":1,"should":[{"term":{"event_people":"U"}},{"terms":{"event_calendar_id":[]}},` +
		`{"bool":{"filter":[{"terms":{"event_calendar_id":[]}}],"must_not":[{"term":{"event_private":true}}]}}]}}`
	if got := eventsClause(t, sent); got != want {
		t.Errorf("with no calendars read, the events' clause\n  got  %s\n  want %s", got, want)
	}
}

// An event's notes, the people it is theirs to see and its guests' names are
// matched, never sent back: who is invited is the event's to show, to those
// its guests may see; a doc's body is left out as before.
func TestAnEventHitNeverCarriesItsNotesPeopleOrGuests(t *testing.T) {
	allReady(t)
	withChecks(t, nil)
	var body map[string]any
	swap(t, &searchIndex, func(_ context.Context, _ []string, raw []byte) (opensearchModels.SearchAnswer, error) {
		_ = json.Unmarshal(raw, &body)
		return opensearchModels.SearchAnswer{}, nil
	})
	if _, err := Search(context.Background(), sample, launch(), Page{}); err != nil {
		t.Fatal(err)
	}
	excludes, _ := json.Marshal(body["_source"])
	for _, field := range []string{"doc_body", "event_notes", "event_people", "event_guest_names"} {
		if !strings.Contains(string(excludes), `"`+field+`"`) {
			t.Errorf("a hit carries %s: _source %s", field, excludes)
		}
	}
}

// An event the live check drops comes off the events' count on that page,
// and has its entry written again from the calendar's store, once a while
// however often it is searched for.
func TestADroppedEventComesOffItsCountAndHasItsEntryRepairedOnce(t *testing.T) {
	allReady(t)
	withChecks(t, nil)
	gone, open := uuid.NewString(), uuid.NewString()
	swap(t, &readableEvents, func(context.Context, accessBusiness.Subject, []string) (map[string]bool, error) {
		return map[string]bool{open: true}, nil
	})
	var repaired []string
	swap(t, &repairEntry, func(kind, id string) { repaired = append(repaired, kind+":"+id) })
	swap(t, &goRepairs, func(repair func()) { repair() })
	swap(t, &searchIndex, func(context.Context, []string, []byte) (opensearchModels.SearchAnswer, error) {
		return opensearchModels.SearchAnswer{
			Hits:    []*openSearchStruct.GlobalSearchOpenSearchResp{eventHit(gone), eventHit(open)},
			ByIndex: map[string]int{openSearchStruct.EVENT_INDEX: 2},
			Total:   2,
		}, nil
	})
	for range 3 {
		p, err := Search(context.Background(), Reach{UserUUID: "U"}, launch(), Page{Size: 5})
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Page) != 1 || p.Page[0].Event.Uuid != open || p.Totals[KindEvent] != 1 || p.Total != 1 {
			t.Fatalf("page %d, totals %v, total %d; want the open event alone, events 1, 1 in all", len(p.Page), p.Totals, p.Total)
		}
	}
	if want := []string{"event:" + gone}; !reflect.DeepEqual(repaired, want) {
		t.Errorf("repaired %v; want %v, once", repaired, want)
	}
}
