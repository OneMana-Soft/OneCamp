package business

import (
	"testing"

	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/google/uuid"
)

// Who may change a workflow is helpers.OwnerOrAdmin's answer: one with no
// recorded creator is the admins' alone, and nobody signed in is never its
// creator. The copy here compared the two ids bare.
func TestWorkflowOwnershipIsOwnerOrAdmin(t *testing.T) {
	maker := uuid.New()
	if canManage(Actor{}, &workflowModel.Workflow{}) {
		t.Error("nobody signed in manages a workflow with no recorded creator")
	}
	if canManage(Actor{UserID: maker}, nil) {
		t.Error("no workflow is nobody's to manage")
	}
	if !canManage(Actor{UserID: maker}, &workflowModel.Workflow{CreatedBy: maker}) || canManage(Actor{UserID: uuid.New()}, &workflowModel.Workflow{CreatedBy: maker}) {
		t.Error("its creator manages a workflow, and nobody else but an admin")
	}
	if !canManage(Actor{IsAdmin: true}, &workflowModel.Workflow{}) {
		t.Error("an admin manages any workflow")
	}
}
