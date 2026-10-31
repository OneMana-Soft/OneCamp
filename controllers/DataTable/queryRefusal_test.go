package controllers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	query "github.com/akashc777/OneCamp/business/QuerySpec"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
)

// A view's notice says why the server won't answer it in the refusal's own
// words, never the kind of error it is ("row query: that can't be asked of
// this table yet: grouped this way, …", CR-62).
func TestAQueryRefusalSaysWhyInItsOwnWords(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("%w: %s", rowQuery.ErrCantQuery, "grouped this way, the rows fall into more than 1,000 groups"), "Grouped this way, the rows fall into more than 1,000 groups"},
		{fmt.Errorf("%w: %s", query.ErrBadSpec, "collapse at most 1000 groups a level"), "Collapse at most 1000 groups a level"},
		{rowQuery.ErrBadCursor, "That page marker isn't this query's"},
		{rowQuery.ErrTooManyToWorkOut, "T" + rowQuery.ErrTooManyToWorkOut.Error()[len("row query: t"):]},
	} {
		w := httptest.NewRecorder()
		WriteQueryErr(w, httptest.NewRequest(http.MethodPost, "/tables/t/query", nil), c.err)
		var body struct {
			Msg string `json:"msg"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if w.Code != http.StatusUnprocessableEntity || body.Msg != c.want {
			t.Errorf("%v: answered %d %q, want 422 %q", c.err, w.Code, body.Msg, c.want)
		}
	}
}

// airtable-03b: a formula still being filled for the first time over a big
// table isn't a refusal: the 422 says how far the fill has got, by its code
// and its progress, so the view shows that and asks again; its words still
// say it to a client that reads only msg.
func TestAFillStillRunningSaysHowFarItHasGot(t *testing.T) {
	for _, c := range []struct {
		err  *rowQuery.StillWorkingOut
		want string
	}{
		{&rowQuery.StillWorkingOut{Field: "Double", Done: 42000, Total: 100000}, "Double is being worked out over 100,000 rows: 42% done"},
		{&rowQuery.StillWorkingOut{Field: "Paid", Done: 0, Total: 0}, "Paid is being worked out over every row: starting"},
	} {
		w := httptest.NewRecorder()
		WriteQueryErr(w, httptest.NewRequest(http.MethodPost, "/tables/t/query", nil), fmt.Errorf("sorting: %w", c.err))
		var body struct {
			Msg      string `json:"msg"`
			Code     string `json:"code"`
			Progress *struct {
				Done  int64 `json:"done"`
				Total int64 `json:"total"`
			} `json:"progress"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if w.Code != http.StatusUnprocessableEntity || body.Msg != c.want {
			t.Errorf("%v: answered %d %q, want 422 %q", c.err, w.Code, body.Msg, c.want)
		}
		if body.Code != "working_out" || body.Progress == nil || body.Progress.Done != c.err.Done || body.Progress.Total != c.err.Total {
			t.Errorf("%v: code %q progress %+v, want working_out with %d of %d", c.err, body.Code, body.Progress, c.err.Done, c.err.Total)
		}
	}
}
