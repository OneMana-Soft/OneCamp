package business

import (
	"testing"

	fileAccessBusiness "github.com/akashc777/OneCamp/business/FileAccess"
)

// The call grant reaches a board's pictures through File Access, which can't
// import this package (it would be a cycle), so this package hands it the
// answer.
func TestTheCallGrantReachesABoardsPictures(t *testing.T) {
	if fileAccessBusiness.CallBoard == nil {
		t.Fatal("File Access has no call grant for boards")
	}
}
