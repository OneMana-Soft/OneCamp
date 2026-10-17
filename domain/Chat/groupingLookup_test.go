package domain

import (
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// The repair asks the graph about the grouping ids it finds, as the app
// writes them: a group's from its people, a direct conversation's from its
// two. Anything else stays out of the query.
func TestAGroupingIDIsOneTheAppWrites(t *testing.T) {
	a, b, c := uuid.NewString(), uuid.NewString(), uuid.NewString()
	group, err := helpers.GenerateGroupID([]string{a, b, c})
	if err != nil {
		t.Fatal(err)
	}
	dm := helpers.GetGroupingId(a, b)
	list, n := quotedList([]string{group, dm, "", group[:31], a + " " + b + " " + c, `x", "y`}, isGroupingID)
	if n != 2 || !strings.Contains(list, `"`+group+`"`) || !strings.Contains(list, `"`+dm+`"`) {
		t.Errorf("the grouping ids asked about: %s (%d), want the group's and the conversation's", list, n)
	}
}
