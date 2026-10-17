package helpers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A doc that isn't there for the person is one 404, in one sentence, whether
// it doesn't exist or they can't open it; a read that failed is a 503 to try
// again, never that sentence.
func TestADocReadThatGaveNoAnswerIsSaidOneWay(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/doc", nil)
	read := func(write func(w http.ResponseWriter)) (int, string) {
		w := httptest.NewRecorder()
		write(w)
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		msg, _ := body["msg"].(string)
		return w.Code, msg
	}
	for name, write := range map[string]func(w http.ResponseWriter){
		"no doc": func(w http.ResponseWriter) { WriteDocNotFound(w) },
		"a read that found none": func(w http.ResponseWriter) {
			WriteDocReadFailure(w, r, NotFound("failed to get dgraph doc"), "helpers/test")
		},
		"one wrapped on its way": func(w http.ResponseWriter) {
			WriteDocReadFailure(w, r, fmt.Errorf("deciding: %w", NotFound("x")), "helpers/test")
		},
	} {
		if code, msg := read(write); code != http.StatusNotFound || msg != DocNotFound {
			t.Errorf("%s: %d %q, want 404 %q", name, code, msg, DocNotFound)
		}
	}
	if code, msg := read(func(w http.ResponseWriter) { WriteDocReadFailure(w, r, errors.New("graph down"), "helpers/test") }); code != http.StatusServiceUnavailable || msg != "Couldn't load the doc. Try again in a moment." {
		t.Errorf("a read that failed: %d %q, want 503 to try again", code, msg)
	}
}
