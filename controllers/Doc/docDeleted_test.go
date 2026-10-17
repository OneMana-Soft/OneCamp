package docController

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The doc page's 410 says whether the person may restore the doc, so the page
// offers Restore only to them; it still says the doc was deleted either way.
func TestADeletedDocSaysWhetherItCanBeRestored(t *testing.T) {
	for _, can := range []bool{true, false} {
		rec := httptest.NewRecorder()
		docDeleted(rec, can)
		var body struct {
			Msg        string `json:"msg"`
			Deleted    bool   `json:"deleted"`
			CanRestore bool   `json:"can_restore"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusGone || !body.Deleted || body.CanRestore != can || body.Msg != "This doc was deleted." {
			t.Errorf("can=%v: %d %+v", can, rec.Code, body)
		}
	}
}
