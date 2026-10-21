package Domain

import (
	"strings"
	"testing"
)

// PR3 (the BE audit's payloads): a board card never carries its task's
// description: the board read up to 1,000 cards a column with every
// description in full, which no card shows. The panel reads it.
func TestABoardCardCarriesNoDescription(t *testing.T) {
	if strings.Contains(projectTaskFields, "task_description") {
		t.Fatal("projectTaskFields reads task_description")
	}
}
