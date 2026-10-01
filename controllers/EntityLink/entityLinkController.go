package entityLinkController

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	adapter "github.com/akashc777/OneCamp/adapter/EntityLink"
	business "github.com/akashc777/OneCamp/business/EntityLink"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

func validRefType(t string) bool {
	return t == business.RefDoc || t == business.RefBoard
}

// canEditSource verifies the user may modify links on the given source. Tasks
// require project membership; projects require membership. Returns ok and, on
// failure, whether it was an access denial (vs a lookup error).
func canEditSource(r *http.Request, sourceType, sourceUUID, userDgraphUID string) (ok bool, denied bool) {
	ctx := r.Context()
	switch sourceType {
	case business.SourceTask:
		task, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, sourceUUID, userDgraphUID)
		if err != nil || task == nil || task.Project == nil {
			return false, false
		}
		return task.Project.IsProjectMember > 0, task.Project.IsProjectMember == 0
	case business.SourceProject:
		project, err := projectBusiness.GetBasicDgraphProjectInfo(ctx, sourceUUID, userDgraphUID)
		if err != nil || project == nil {
			return false, false
		}
		return project.IsProjectMember > 0, project.IsProjectMember == 0
	default:
		return false, false
	}
}

// AddLink POST /link/add - link a doc/board to a task/project.
func AddLink(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var input adapter.InputEntityLink
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req", "err": err})
		return
	}
	input.SourceType = strings.TrimSpace(input.SourceType)
	input.RefType = strings.TrimSpace(input.RefType)

	if !validRefType(input.RefType) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid reference type"})
		return
	}
	if _, err := uuid.Parse(input.SourceUUID); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid source id"})
		return
	}
	if _, err := uuid.Parse(input.RefUUID); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid reference id"})
		return
	}

	ok, _ := canEditSource(r, input.SourceType, input.SourceUUID, userInfo.UserDgraphInfo.Uid)
	if !ok {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}

	// You can only link a doc/board you can actually see.
	canRead, err := business.CanReadRef(ctx, input.RefType, input.RefUUID, userInfo.UserDgraphInfo.Uid, userInfo.UserDgraphInfo.Uuid)
	if err != nil || !canRead {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}

	if err := business.AddLink(ctx, input.SourceType, input.SourceUUID, input.RefType, input.RefUUID); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/AddLink failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to add link", "err": err.Error()})
		return
	}

	// Return the fresh, access-filtered link list so the client updates from
	// the response and never needs a follow-up fetch.
	docs, boards, _, err := business.GetLinksForSource(ctx, input.SourceType, input.SourceUUID, userInfo.UserDgraphInfo.Uid, userInfo.UserDgraphInfo.Uuid)
	if err != nil {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "linked successfully!"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "linked successfully!", "data": map[string]interface{}{"docs": docs, "boards": boards}})
}

// RemoveLink POST /link/remove - unlink a doc/board from a task/project.
func RemoveLink(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var input adapter.InputEntityLink
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req", "err": err})
		return
	}
	input.SourceType = strings.TrimSpace(input.SourceType)
	input.RefType = strings.TrimSpace(input.RefType)

	if !validRefType(input.RefType) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid reference type"})
		return
	}
	if _, err := uuid.Parse(input.SourceUUID); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid source id"})
		return
	}
	if _, err := uuid.Parse(input.RefUUID); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid reference id"})
		return
	}

	ok, _ := canEditSource(r, input.SourceType, input.SourceUUID, userInfo.UserDgraphInfo.Uid)
	if !ok {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}

	if err := business.RemoveLink(ctx, input.SourceType, input.SourceUUID, input.RefType, input.RefUUID); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/RemoveLink failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to remove link", "err": err.Error()})
		return
	}

	docs, boards, _, err := business.GetLinksForSource(ctx, input.SourceType, input.SourceUUID, userInfo.UserDgraphInfo.Uid, userInfo.UserDgraphInfo.Uuid)
	if err != nil {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "unlinked successfully!"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "unlinked successfully!", "data": map[string]interface{}{"docs": docs, "boards": boards}})
}

// 403 AND NOT 401 THROUGHOUT THIS FILE. Every refusal here is an authorisation
// outcome on a route that has already authenticated the caller: we know exactly
// who they are, and they are not a member of the project behind this task. 401
// says "who are you", and the frontend's axios interceptor believes it: it fires
// a token refresh and re-issues the request, and if that refresh fails in the
// window it logs the user out. So opening a task in a project you are not in
// used to cost a spurious refresh plus a duplicate request, over a permissions
// answer that was never going to change.
//
// GetSourceLinks GET /link/source/{source_type}/{source_uuid} - docs and boards
// a task/project links (filtered to what the viewer can open).
func GetSourceLinks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	sourceType := strings.TrimSpace(chi.URLParam(r, "source_type"))
	sourceUUID := strings.TrimSpace(chi.URLParam(r, "source_uuid"))

	if sourceType != business.SourceTask && sourceType != business.SourceProject {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid source type"})
		return
	}
	if _, err := uuid.Parse(sourceUUID); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid source id"})
		return
	}

	// Single round-trip: the query both authorises (membership) and returns
	// the access-filtered links.
	docs, boards, isMember, err := business.GetLinksForSource(ctx, sourceType, sourceUUID, userInfo.UserDgraphInfo.Uid, userInfo.UserDgraphInfo.Uuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetSourceLinks failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to get links", "err": err.Error()})
		return
	}
	if !isMember {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Success", "data": map[string]interface{}{"docs": docs, "boards": boards}})
}

// GetRefLinks GET /link/ref/{ref_type}/{ref_uuid} - tasks and projects that
// link a given doc/board (filtered to the viewer's projects).
func GetRefLinks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	refType := strings.TrimSpace(chi.URLParam(r, "ref_type"))
	refUUID := strings.TrimSpace(chi.URLParam(r, "ref_uuid"))

	if !validRefType(refType) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid reference type"})
		return
	}
	if _, err := uuid.Parse(refUUID); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid reference id"})
		return
	}

	// Only someone who can open the doc/board may see what links to it.
	canRead, err := business.CanReadRef(ctx, refType, refUUID, userInfo.UserDgraphInfo.Uid, userInfo.UserDgraphInfo.Uuid)
	if err != nil || !canRead {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}

	tasks, projects, err := business.GetReverseLinksForRef(ctx, refType, refUUID, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetRefLinks failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to get links", "err": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Success", "data": map[string]interface{}{"tasks": tasks, "projects": projects}})
}
