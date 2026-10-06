package business

import (
	"strings"
	"testing"
)

func TestProjectUpdatePrompt(t *testing.T) {
	p := projectUpdatePrompt("Q4 launch", "Since Mon: 2 done.", []string{"Shipped SSO.", "Slipped a day."})
	for _, want := range []string{"Project: Q4 launch", "Facts:\nSince Mon: 2 done.", "Previous updates, newest first:", "--- 1 ---\nShipped SSO.", "--- 2 ---\nSlipped a day."} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(projectUpdatePrompt("P", "F", nil), "Previous updates") {
		t.Error("a first update has no previous ones to imitate")
	}
}
