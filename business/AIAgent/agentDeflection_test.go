package business

import "testing"

func TestLooksLikeDeflection(t *testing.T) {
	for _, s := range []string{
		"I’m ready for the next step. Let me know what you’d like to do next.",
		"Ready for your next instruction.",
		"I’ve reviewed the recent activity. No new action is required at this time.",
		"How can I help?",
	} {
		if !looksLikeDeflection(s) {
			t.Fatalf("missed a hand-back: %q", s)
		}
	}
	for _, s := range []string{
		"The rollback steps are already in the Launch sync notes under Decisions: redeploy the previous tag, then restore the pre-release backup. Maya added them this morning, so nothing needed doing. Let me know what you'd like changed.",
		"Added the two rollback steps under Decisions in the Launch sync notes.",
		"",
	} {
		if looksLikeDeflection(s) {
			t.Fatalf("a real answer was taken for a hand-back: %q", s)
		}
	}
}
