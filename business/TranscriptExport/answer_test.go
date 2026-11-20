package business

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// answerOnce runs Answer for query q and a page of 2 from line 4, and reports
// every page read was asked for, what Answer handed back, and the response.
func answerOnce(t *testing.T, q string, rec *dgraphStruct.DgraphRecording) ([]helpers.Page, *dgraphStruct.DgraphRecording, *httptest.ResponseRecorder) {
	t.Helper()
	if _, err := url.ParseQuery(q); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	var asked []helpers.Page
	got := Answer(w, httptest.NewRequest(http.MethodGet, "/transcript?"+q, nil), helpers.Page{Index: 4, Size: 2}, func(p helpers.Page) (*dgraphStruct.DgraphRecording, string) {
		asked = append(asked, p)
		return rec, "standup"
	})
	return asked, got, w
}

func TestAnswerReadsOnceForAPageOrTheWholeExport(t *testing.T) {
	rec := &dgraphStruct.DgraphRecording{}

	asked, got, w := answerOnce(t, "pageSize=2", rec)
	if len(asked) != 1 || asked[0] != (helpers.Page{Index: 4, Size: 2}) || got != rec || w.Body.Len() != 0 {
		t.Fatalf("a page read: asked %v, handed back %v, wrote %q; want one read of the page asked, the recording back, nothing written", asked, got, w.Body.String())
	}

	asked, got, w = answerOnce(t, "format=txt", rec)
	if len(asked) != 1 || asked[0] != (helpers.Page{Size: MaxLines}) || got != nil {
		t.Fatalf("an export: asked %v, handed back %v; want one capped read from the first line and nil back", asked, got)
	}
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("an export answered %d %q; want the file", w.Code, w.Header().Get("Content-Disposition"))
	}

	asked, got, w = answerOnce(t, "format=doc", rec)
	if len(asked) != 0 || got != nil || w.Code != http.StatusBadRequest {
		t.Fatalf("an unknown format: asked %v, handed back %v, answered %d; want no read, nil back, 400", asked, got, w.Code)
	}

	// read answered the request itself (the missing answer): nothing more is written.
	asked, got, w = answerOnce(t, "format=vtt", nil)
	if len(asked) != 1 || got != nil || w.Body.Len() != 0 || w.Header().Get("Content-Disposition") != "" {
		t.Fatalf("a refused export: asked %v, handed back %v, wrote %q; want one read and nothing written", asked, got, w.Body.String())
	}
}
