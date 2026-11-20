package models

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
)

// M4: a transcript entry reads into a hit carrying the moment to play.
func TestATranscriptEntryCarriesTheMomentToPlay(t *testing.T) {
	h, err := decodeHit(openSearchStruct.TRANSCRIPT_INDEX, []byte(`{"transcript_id":"0x9","transcript_text":"ship it","transcript_offset_ms":4200,
		"transcript_recording_id":"EG_1","transcript_has_media":true,"transcript_conv_id":"ch-1","transcript_conv_kind":"channel",
		"transcript_by_user_id":"u-1","transcript_call_started_at":1763000000,"created_date":1763000004}`))
	if err != nil || h == nil || h.Type != openSearchStruct.TRANSCRIPT_TYPE || h.Transcript == nil {
		t.Fatalf("decoded %+v, %v", h, err)
	}
	if l := h.Transcript; l.Uuid != "0x9" || l.OffsetMs != 4200 || l.RecordingID != "EG_1" || !l.HasMedia || l.ConvID != "ch-1" ||
		l.ConvKind != "channel" || l.ByUserUuid != "u-1" || l.CallStartedAt != 1763000000 {
		t.Errorf("the hit = %+v", l)
	}
}

// A missing index (an install that predates it) fails no search of the rest.
func TestASearchAsksOverIndicesThatMayBeMissing(t *testing.T) {
	var query string
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		fmt.Fprint(w, `{"hits":{"total":{"value":0},"hits":[]}}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	defer func() { opensearchInit.OpenSearchClient = nil }()
	if _, err := SearchPage(context.Background(), []string{"posts", openSearchStruct.TRANSCRIPT_INDEX}, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ignore_unavailable=true", "allow_no_indices=true"} {
		if !strings.Contains(query, want) {
			t.Errorf("the search asked %q; want %s", query, want)
		}
	}
}
