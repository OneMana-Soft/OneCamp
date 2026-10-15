package business

import (
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

// The speech-to-text key stays with the address it was given for, and only
// for the providers whose key the form manages: switching to the bundled
// server or to Google, which the form asks no key for, must never be stopped
// by a key it can't enter.
func TestTheSpeechToTextKeyStaysWithItsAddress(t *testing.T) {
	t.Setenv("STT_BASE_URL", "")
	str := func(s string) *string { return &s }
	cases := []struct {
		why         string
		curURL, cur string
		in          SaveInput
		refused     bool
	}{
		{"to the bundled server, the controller clearing the key", "", STTDeepgram, SaveInput{STTProvider: str(STTLocal), STTAPIKey: str("")}, false},
		{"to Google, which takes its own credentials", "", STTDeepgram, SaveInput{STTProvider: str(STTGoogle), STTBaseURL: str("")}, false},
		{"another OpenAI-compatible address, no key", "https://whisper.example.com/v1", STTOpenAI, SaveInput{STTProvider: str(STTOpenAI), STTBaseURL: str("https://elsewhere.example.net/v1")}, true},
		{"another OpenAI-compatible address, with its key", "https://whisper.example.com/v1", STTOpenAI, SaveInput{STTProvider: str(STTOpenAI), STTBaseURL: str("https://elsewhere.example.net/v1"), STTAPIKey: str("new")}, false},
		{"from an OpenAI-compatible address to Deepgram, no key", "https://whisper.example.com/v1", STTOpenAI, SaveInput{STTProvider: str(STTDeepgram), STTBaseURL: str("")}, true},
		{"Deepgram again, nothing moved", "", STTDeepgram, SaveInput{STTProvider: str(STTDeepgram), STTBaseURL: str("")}, false},
		{"another path on the same address", "https://whisper.example.com/v1", STTOpenAI, SaveInput{STTBaseURL: str("https://whisper.example.com/v2")}, false},
	}
	for _, c := range cases {
		err := checkSTTKeyStays(true, c.curURL, c.cur, c.in)
		if got := helpers.IsSecretReentry(err); got != c.refused {
			t.Errorf("%s: %v; want refused=%v", c.why, err, c.refused)
		}
	}
	if err := checkSTTKeyStays(false, "https://whisper.example.com/v1", STTOpenAI, SaveInput{STTBaseURL: str("https://elsewhere.example.net/v1")}); err != nil {
		t.Errorf("no key stored, nothing to carry: %v", err)
	}
}
