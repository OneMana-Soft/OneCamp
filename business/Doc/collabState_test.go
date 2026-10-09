package businness

import "testing"

// The collaboration service opens a doc from its saved Yjs state only when the
// stored body hashes to what was saved beside it. Its bodyHash (docState.js,
// pinned to the same value in docState.test.js) and this must agree, or every
// open rebuilds from the body and the doubling it prevents comes back.
func TestTheBodyHashAgreesWithTheCollaborationService(t *testing.T) {
	const body = "<p>Ship on Thursday. Café ✓</p>"
	const want = "aa0219c1b9ca040a1d19798de36fcb4ff57ccfc02209bcf492e615a1cd72c7af"
	if got := collabBodyHash(body); got != want {
		t.Fatalf("collabBodyHash = %s, want %s", got, want)
	}
}
