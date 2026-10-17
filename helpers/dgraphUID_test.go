package helpers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A uid is "0x" and up to 16 hex digits in either case: Dgraph reads "0xAB" as
// it reads "0xab". The release audit (S4) found this check and dgraphquery's
// differing on case; there is one check now.
func TestIsDgraphUID(t *testing.T) {
	for s, want := range map[string]bool{
		"0x1": true, "0xab12": true, "0xffffffffffffffff": true, "0xAB": true, "0xaB12": true, "0xFFFFFFFFFFFFFFFF": true,
		"": false, "0x": false, "1": false, "0xG1": false, "0XAB": false, "0x 1": false, "0x1\n": false,
		"0x1) OR has(x": false, "0x1ffffffffffffffff": false, "guest-0x1": false,
	} {
		if got := IsDgraphUID(s); got != want {
			t.Errorf("IsDgraphUID(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestWriteRetryLaterAnswers503WithAdvice(t *testing.T) {
	w := httptest.NewRecorder()
	WriteRetryLater(w, "The call server couldn't answer just now.")
	var body struct{ Msg string }
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusServiceUnavailable || body.Msg != "The call server couldn't answer just now. Try again in a moment." {
		t.Errorf("got %d %q", w.Code, body.Msg)
	}
}
