package business

import "testing"

func TestLooksLikeNonAnswer(t *testing.T) {
	nonAnswers := []string{
		// The exact production failure shape.
		"To analyze the commits I will first search for the repository and then search for commits. " +
			"Please wait for the tool results to provide a detailed summary. Assuming the tool results are available, " +
			"here is a concise reply: \"We shipped the following changes: [list of commits].\"",
		"I'll get back to you with the summary shortly.",
		"Once I have the results I will summarize them.",
		"We shipped: [list of commits]",
		"Here is a concise reply: the answer is coming.",
		"I will search and reply with what I find.",
	}
	for _, s := range nonAnswers {
		if !looksLikeNonAnswer(s) {
			t.Errorf("expected non-answer to be flagged: %q", s)
		}
	}

	realAnswers := []string{
		"",
		"Here's what I found: the checkout deploy is on staging and payment latency dropped 40%.",
		"I can help with that. The refund SLA is 5 business days.",
		"Done — I created the task and assigned it to Daniel.",
		"The repo has 3 open PRs: #12 checkout, #14 refunds, #15 docs.",
		"See reference [1] for details.", // short bracket, not a placeholder
	}
	for _, s := range realAnswers {
		if looksLikeNonAnswer(s) {
			t.Errorf("real answer wrongly flagged as non-answer: %q", s)
		}
	}
}
