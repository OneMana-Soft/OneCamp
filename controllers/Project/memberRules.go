package controllers

import (
	"net/http"
	"strings"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// managesMembers is whether the caller, for whom p was read, may change who is
// in the project and who runs it: one of its admins, or an admin of its team.
// Adding and removing people and admins all ask it. Pure.
func managesMembers(p *dgraphStruct.DgraphProject) bool {
	return p != nil && (p.IsProjectAdmin > 0 || (p.Team != nil && p.Team.IsAdmin > 0))
}

// memberRemovalRefusal is why the caller (by user id) may not take memberUUID
// off the project, as a status and what to say, or 0 when they may: those who
// manage its people (managesMembers) may take anyone off but its creator, and
// anyone may leave. p is the project read for the caller with the member
// asked about (Members holds them when they're in it). A project the caller
// can't see, and someone who isn't in it, answer as not found. Before, the only
// check was on the member: anyone signed in could take any member but the
// creator off any project, in it or not. Pure.
func memberRemovalRefusal(p *dgraphStruct.DgraphProject, callerUUID, memberUUID string) (int, string) {
	if p == nil || p.Uuid == "" {
		return http.StatusNotFound, "Project not found"
	}
	if !managesMembers(p) && !strings.EqualFold(callerUUID, memberUUID) {
		if p.IsProjectMember == 0 {
			return http.StatusNotFound, "Project not found"
		}
		return http.StatusForbidden, "Only the project's admins can take people off it."
	}
	if len(p.Members) == 0 {
		return http.StatusNotFound, "They aren't in this project."
	}
	if p.CreatedBy != nil && p.CreatedBy.Uid == p.Members[0].Uid {
		return http.StatusForbidden, "The project's creator can't be taken off it."
	}
	return 0, ""
}
