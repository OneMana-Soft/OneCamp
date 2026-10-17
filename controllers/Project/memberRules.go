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

// memberChange is a change to who is in a project, or who runs it.
type memberChange int

const (
	addMember memberChange = iota
	takeOff
	makeAdmin
	removeAdmin
)

// refusedWords is what a member who doesn't manage the project's people is
// told for each change: who can make it.
var refusedWords = map[memberChange]string{
	addMember:   "Only the project's admins can add people to it.",
	takeOff:     "Only the project's admins can take people off it.",
	makeAdmin:   "Only the project's admins can make someone an admin.",
	removeAdmin: "Only the project's admins can change who its admins are.",
}

// doneWords is what each change says once it is made.
var doneWords = map[memberChange]string{
	addMember:   "Added to the project.",
	takeOff:     "Taken off the project.",
	makeAdmin:   "Made an admin of the project.",
	removeAdmin: "No longer an admin of the project.",
}

// memberChangeRefusal is why the caller (by user id) may not make the change
// to memberUUID, as a status and what to say, or 0 when they may. Those who
// manage the project's people (managesMembers) make every change, and anyone
// may leave; someone in the project who doesn't manage it is told who can.
// p is the project read for the caller with the person asked about: Members
// holds them when they're in the project, Team.Members when they're in its
// team. A project the caller can't see, and a person who isn't there, answer
// as not found; the creator is never taken off it or out of its admins. Pure.
//
// Taking people off checked only the member until PROJ-BUG-5, and the other
// three answered "Not Authorised" to everyone they refused, an admin asked
// about someone who isn't there included.
func memberChangeRefusal(p *dgraphStruct.DgraphProject, change memberChange, callerUUID, memberUUID string) (int, string) {
	if p == nil || p.Uuid == "" {
		return http.StatusNotFound, "Project not found"
	}
	leaving := change == takeOff && strings.EqualFold(callerUUID, memberUUID)
	if !managesMembers(p) && !leaving {
		if p.IsProjectMember == 0 {
			return http.StatusNotFound, "Project not found"
		}
		return http.StatusForbidden, refusedWords[change]
	}
	if change == addMember {
		if p.Team == nil || len(p.Team.Members) == 0 {
			return http.StatusNotFound, "They aren't in the project's team."
		}
		if p.Team.Members[0].IsExternal {
			return http.StatusBadRequest, "A guest can't join a project. Share it with them instead."
		}
		return 0, ""
	}
	if len(p.Members) == 0 {
		return http.StatusNotFound, "They aren't in this project."
	}
	if p.CreatedBy != nil && p.CreatedBy.Uid == p.Members[0].Uid {
		switch change {
		case takeOff:
			return http.StatusForbidden, "The project's creator can't be taken off it."
		case removeAdmin:
			return http.StatusForbidden, "The project's creator stays one of its admins."
		}
	}
	return 0, ""
}
