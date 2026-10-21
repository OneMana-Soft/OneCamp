package business

import (
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// Who may change a table is helpers.OwnerOrAdmin's answer: a table with no
// recorded creator (a guest bundle's reads it as zero) is the admins' alone,
// and nobody signed in is never its creator. The copy here compared the two
// ids bare, so a zero actor "owned" every table with a zero creator.
func TestTableOwnershipIsOwnerOrAdmin(t *testing.T) {
	maker := uuid.New()
	nobody := Actor{}
	if canManage(&model.DataTable{}, nobody) {
		t.Error("nobody signed in manages a table with no recorded creator")
	}
	if canView(&model.DataTable{Visibility: "private"}, nobody) {
		t.Error("nobody signed in sees a private table with no recorded creator")
	}
	if !canManage(&model.DataTable{CreatedBy: maker}, Actor{UserID: maker}) {
		t.Error("its creator manages a table")
	}
	if canManage(&model.DataTable{CreatedBy: maker}, Actor{UserID: uuid.New()}) {
		t.Error("someone else does not")
	}
	if !canManage(&model.DataTable{}, Actor{IsAdmin: true}) {
		t.Error("an admin manages any table")
	}
	if !canView(&model.DataTable{Visibility: model.VisibilityWorkspace}, Actor{UserID: uuid.New()}) {
		t.Error("a workspace table is everyone's to see")
	}
}
