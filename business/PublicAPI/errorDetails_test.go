package business

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A refusal's details, when it has them (the rows a batch refused), go in the
// one error body beside its code and words; one without them has no details.
func TestARefusalsDetailsGoInTheOneErrorBody(t *testing.T) {
	e := Errorf(http.StatusBadRequest, "invalid_argument", "fix the rows it names", "2 rows can't be written")
	e.Details = []map[string]string{{"row": "r1", "message": "Points takes a number"}}
	rec := httptest.NewRecorder()
	writeError(rec, e)
	var out struct {
		Error struct {
			Code    string              `json:"code"`
			Details []map[string]string `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusBadRequest ||
		out.Error.Code != "invalid_argument" || len(out.Error.Details) != 1 || out.Error.Details[0]["row"] != "r1" {
		t.Errorf("a refusal with details wrote %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	writeError(rec, Errorf(http.StatusNotFound, "not_found", "", "there is no such row"))
	if strings.Contains(rec.Body.String(), "details") {
		t.Errorf("a refusal without details wrote %s", rec.Body.String())
	}
}
