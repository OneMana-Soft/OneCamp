package business

import "testing"

// The reply Release Captain gave on the demo after being assigned "add a
// Status section to the Launch sync notes", having read everything it needed.
const demoOfferReply = `The current task list shows the highest-priority items:

1. **Write the launch announcement** - *inProgress*, due Sep 30 2026 (today).
2. **Set up SSO for the pilot customer** - *QA (inReview)*, due Oct 2 2026.

Would you like me to:

- Update the status of the launch announcement,
- Add a new task, or
- Provide a brief plan for the next steps?`

func TestHandsBackWork(t *testing.T) {
	cases := []struct {
		name                       string
		text                       string
		delegated, attemptedChange bool
		want                       bool
	}{
		{"delegated, nothing changed, ends with an offer", demoOfferReply, true, false, true},
		{"the same in a conversation is a fine ending", demoOfferReply, false, false, false},
		{"delegated but it already made (or proposed) the change", demoOfferReply, true, true, false},
		{"stock hand-back anywhere", "Done reading. Ready for the next step.", false, true, true},
		{"a report that asks nothing", "Added the Status section: 4 lines, one per open task.", true, false, false},
		{"a question that offers nothing", "I couldn't find the Launch sync notes. Is it the doc called Launch notes?", true, false, false},
		{"bare acknowledgement in delegated work", "Got it.", true, false, true},
		{"bare acknowledgement in a conversation", "Got it.", false, false, false},
		{"short reply after making the change", "Done.", true, true, false},
		{"curly apostrophe offer", "Checked all four tasks. If you’d like, I can add them to the notes?", true, false, true},
	}
	for _, c := range cases {
		if got := handsBackWork(c.text, c.delegated, c.attemptedChange); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestEndsWithOfferReadsOnlyTheEnd(t *testing.T) {
	long := "Would you like me to check? " + string(make([]rune, 0))
	for i := 0; i < 80; i++ {
		long += "Task line with plenty of detail. "
	}
	long += "All four tasks are listed above."
	if endsWithOffer(long) {
		t.Fatal("an offer early in a long report is not how it ends")
	}
}
