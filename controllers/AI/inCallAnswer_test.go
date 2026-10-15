package controllers

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// CALL-BUG-14. The finished answer is sent to the call by the server, naming
// the question and who asked it, so no participant can replace the
// assistant's answer to someone else's question, or write one for it.
func TestTheInCallAnswerNamesItsQuestionAndAsker(t *testing.T) {
	var got map[string]any
	if err := json.Unmarshal(inCallAnswerPacket("q-1", "The budget is 10k.", "", "0x1"), &got); err != nil {
		t.Fatal(err)
	}
	if got["v"] != float64(1) || got["t"] != "a" || got["id"] != "q-1" || got["answer"] != "The budget is 10k." || got["askedById"] != "0x1" {
		t.Errorf("packet = %v", got)
	}
	if _, has := got["error"]; has {
		t.Errorf("an answer carries no error: %v", got)
	}

	long := strings.Repeat("é", inCallAnswerMaxBytes)
	var big map[string]any
	if err := json.Unmarshal(inCallAnswerPacket("q-2", long, "", "0x1"), &big); err != nil {
		t.Fatal(err)
	}
	answer, _ := big["answer"].(string)
	if len(answer) > inCallAnswerMaxBytes || !utf8.ValidString(answer) {
		t.Errorf("a long answer is %d bytes, valid %v", len(answer), utf8.ValidString(answer))
	}
}

func TestQuestionIDShape(t *testing.T) {
	for id, ok := range map[string]bool{
		"1760356800000-k3j2h1":  true,
		"a.b_c-d":               true,
		"":                      false,
		"has space":             false,
		strings.Repeat("x", 65): false,
		"<script>":              false,
	} {
		if questionID.MatchString(id) != ok {
			t.Errorf("question id %q: valid %v, want %v", id, !ok, ok)
		}
	}
}
