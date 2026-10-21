package business

import (
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/DataSource"
	"github.com/google/uuid"
)

// Who may change or query a data source is helpers.OwnerOrAdmin's answer: one
// with no recorded creator is the admins' alone, and nobody signed in is
// never its creator. The copy here compared the two ids bare.
func TestDataSourceOwnershipIsOwnerOrAdmin(t *testing.T) {
	maker := uuid.New()
	if canManage(&model.DataSource{}, Actor{}) {
		t.Error("nobody signed in manages a source with no recorded creator")
	}
	if canQuery(&model.DataSource{Visibility: "private"}, Actor{}) {
		t.Error("nobody signed in queries a private source with no recorded creator")
	}
	if !canManage(&model.DataSource{CreatedBy: maker}, Actor{UserID: maker}) || canManage(&model.DataSource{CreatedBy: maker}, Actor{UserID: uuid.New()}) {
		t.Error("its creator manages a source, and nobody else but an admin")
	}
	if !canManage(&model.DataSource{}, Actor{IsAdmin: true}) {
		t.Error("an admin manages any source")
	}
	if !canQuery(&model.DataSource{Visibility: model.VisibilityWorkspace}, Actor{UserID: uuid.New()}) {
		t.Error("a workspace source is every member's to query")
	}
}
