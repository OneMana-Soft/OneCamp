package docController

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	treeBusiness "github.com/akashc777/OneCamp/business/Tree"
)

// A sharing change applied to the pages under one, when there are more under it
// than one change takes, is refused whole and said so, as the tree's own
// refusals of a subtree are (a delete, a copy): never "something went wrong".
func TestASharingChangeToTooManyPagesIsRefusedAsTooMany(t *testing.T) {
	for _, answer := range []struct {
		name string
		fail func(w http.ResponseWriter, err error)
	}{
		{"sharing", func(w http.ResponseWriter, err error) { accessFailed(context.Background(), w, "UpdateDocAccess", err) }},
		{"tree", func(w http.ResponseWriter, err error) {
			treeFailed(w, httptest.NewRequest(http.MethodPost, "/doc/duplicate", nil), "DuplicateDoc", err)
		}},
	} {
		rec := httptest.NewRecorder()
		answer.fail(rec, fmt.Errorf("apply: %w", treeBusiness.ErrTooMany))
		var body struct {
			Code string `json:"code"`
			Msg  string `json:"msg"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusUnprocessableEntity || body.Code != "too_many" ||
			body.Msg != "There are too many pages under this one to do that at once." {
			t.Errorf("%s: %d %+v", answer.name, rec.Code, body)
		}
	}
}
