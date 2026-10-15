package helpers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsDgraphUID(t *testing.T) {
	for s, want := range map[string]bool{
		"0x1": true, "0xab12": true, "0xffffffffffffffff": true,
		"": false, "0x": false, "1": false, "0xG1": false, "0xAB": false,
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
