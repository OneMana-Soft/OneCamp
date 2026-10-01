package business

import "testing"

func TestInternalIDElicitation(t *testing.T) {
	asks := []string{
		"I need the UUID of the Q4 launch project. Could you provide the project UUID?",
		"What is the id of the project you mean?",
		"Please share the channel identifier.",
	}
	for _, q := range asks {
		if !internalIDElicitation(q) {
			t.Errorf("should refuse: %q", q)
		}
	}
	fine := []string{
		"Which project did you mean: Q4 launch or Website refresh?",
		"Should I create the task now or wait for the load test numbers?",
		"Do you want me to post this in #engineering?",
	}
	for _, q := range fine {
		if internalIDElicitation(q) {
			t.Errorf("should allow: %q", q)
		}
	}
}
