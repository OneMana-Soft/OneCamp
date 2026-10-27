package business

import (
	"net/http"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// RecordMemberChange keeps a change to a project's people in the audit log
// and tells its Resource Event (project.member.added, project.member.removed,
// or project.changed with the person and their role), as whoever made the
// request. The project's own routes and an admin's import review both call it.
func RecordMemberChange(r *http.Request, kind resourceEventBusiness.Kind, action, summary, projectUUID, memberUUID string, role workAccess.Role) {
	auditBusiness.Record(r, action, auditBusiness.CategorySecurity, summary,
		map[string]interface{}{"project_uuid": projectUUID, "user_uuid": memberUUID, "role": string(role)})
	u, _ := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	data := map[string]string{"member": memberUUID}
	if role != workAccess.RoleNone {
		data["role"] = string(role)
	}
	resourceEventBusiness.Bus.Emit(r.Context(), resourceEventBusiness.Event{
		Kind: kind, ResourceKind: workAccess.KindProject, ResourceUUID: projectUUID,
		Actor: resourceEventBusiness.Actor{UserUUID: u.UserDgraphInfo.Uuid}, At: time.Now(),
		Subjects: []string{memberUUID}, Data: data,
	})
}

// IsCreatorOf says whether the person (by graph uid) made the project: its
// creator can't be taken off it, and stays one of its admins.
func IsCreatorOf(p *dgraphStruct.DgraphProject, personUID string) bool {
	return p != nil && p.CreatedBy != nil && p.CreatedBy.Uid == personUID
}
