package controllers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// GET /command/giphy refuses what Giphy can't serve, before it looks for the
// workspace's key.
func TestGiphyPickerRefusesBadInput(t *testing.T) {
	for name, query := range map[string]string{
		"an offset that isn't a number": "offset=two",
		"a negative offset":             "offset=-24",
		"an offset past Giphy's last":   "offset=5000",
		"a search past 50 characters":   "q=" + strings.Repeat("a", 51),
	} {
		rec := httptest.NewRecorder()
		GiphyPicker(rec, httptest.NewRequest(http.MethodGet, "/command/giphy?"+query, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, rec.Code)
		}
	}
}
