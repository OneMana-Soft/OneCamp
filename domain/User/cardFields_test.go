package domain

import (
	"strings"
	"testing"
)

// PR3: a card on My Tasks' board never carries its task's description.
func TestAMyTasksCardCarriesNoDescription(t *testing.T) {
	if strings.Contains(userTaskCardFields, "task_description") {
		t.Fatal("userTaskCardFields reads task_description")
	}
}
