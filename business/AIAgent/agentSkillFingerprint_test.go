package business

import (
	"encoding/json"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// What has to hold: a run that used no skills says so plainly rather than
// storing nothing, and a run that used some records enough to identify which
// version of each one it was given.
func TestSkillsUsedJSON(t *testing.T) {
	// The column is NOT NULL and "no skills" is a fact worth recording, so the
	// empty case is "[]" and never "".
	if got := skillsUsedJSON(nil); got != "[]" {
		t.Fatalf("no skills rendered as %q, want []", got)
	}
	if got := skillsUsedJSON([]SkillFingerprint{}); got != "[]" {
		t.Fatalf("empty slice rendered as %q, want []", got)
	}

	id := uuid.New()
	body := "Always name the channel once, never twice."
	out := skillsUsedJSON([]SkillFingerprint{{Id: id, Name: "Tone", SHA: helpers.SHA256Hex(body)}})

	var back []SkillFingerprint
	if err := json.Unmarshal([]byte(out), &back); err != nil {
		t.Fatalf("stored value is not valid JSON: %v", err)
	}
	if len(back) != 1 || back[0].Id != id || back[0].Name != "Tone" {
		t.Fatalf("round trip lost the skill: %+v", back)
	}
	// The fingerprint has to match the text, or it proves nothing.
	if back[0].SHA != helpers.SHA256Hex(body) {
		t.Fatal("fingerprint does not match the instructions it was taken from")
	}
	// And it has to differ once the text does, which is the case the whole
	// record exists for: somebody edited a shared skill after the run.
	if back[0].SHA == helpers.SHA256Hex(body+" and be brief") {
		t.Fatal("an edited skill produced the same fingerprint")
	}
}
