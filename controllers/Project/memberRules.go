package controllers

import (
	"net/http"
	"strings"

	principalBusiness "github.com/akashc777/OneCamp/business/Principal"
	business "github.com/akashc777/OneCamp/business/Project"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// memberChange is a change to who is in a project, or what they may do there.
type memberChange int

const (
	addMember memberChange = iota
	takeOff
	makeAdmin
	removeAdmin
	changeRole
)

// refusedWords is what someone in the project who doesn't manage its people
// is told for each change: who can make it.
var refusedWords = map[memberChange]string{
	addMember:   "Only the project's admins can add people to it.",
	takeOff:     "Only the project's admins can take people off it.",
	makeAdmin:   "Only the project's admins can make someone an admin.",
	removeAdmin: "Only the project's admins can change who its admins are.",
	changeRole:  "Only the project's admins can change what people can do in it.",
}

// doneWords is what each change says once it is made.
var doneWords = map[memberChange]string{
	addMember:   "Added to the project.",
	takeOff:     "Taken off the project.",
	makeAdmin:   "Made an admin of the project.",
	removeAdmin: "No longer an admin of the project.",
	changeRole:  "Their role is changed.",
}

// memberChangeRefusal is why the caller (by user id) may not make the change
// to memberUUID, as a status and what to say, or 0 when they may. manages is
// whether Work Access lets them manage the project's people (its admins, and
// its team's admins); anyone may leave; someone in the project who doesn't
// manage it is told who can. p is the project read for the caller with the
// person asked about: Members holds them when they're in the project,
// Team.Members when they're in its team. A project the caller can't see, and
// a person who isn't there, answer as not found; the creator is never taken
// off it or out of its admins. Pure.
//
// Taking people off checked only the member until PROJ-BUG-5, and the other
// three answered "Not Authorised" to everyone they refused, an admin asked
// about someone who isn't there included.
func memberChangeRefusal(p *dgraphStruct.DgraphProject, manages bool, change memberChange, callerUUID, memberUUID string) (int, string) {
	if p == nil || p.Uuid == "" {
		return http.StatusNotFound, "Project not found"
	}
	leaving := change == takeOff && strings.EqualFold(callerUUID, memberUUID)
	if !manages && !leaving {
		if !p.ReaderIsIn() {
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
		if err := principalBusiness.Unassignable(p.Team.Members[0], principalBusiness.Added); err != nil {
			return http.StatusConflict, err.Error()
		}
		return 0, ""
	}
	if len(p.Members) == 0 {
		return http.StatusNotFound, "They aren't in this project."
	}
	if business.IsCreatorOf(p, p.Members[0].Uid) {
		switch change {
		case takeOff:
			return http.StatusForbidden, "The project's creator can't be taken off it."
		case removeAdmin, changeRole:
			return http.StatusForbidden, "The project's creator stays one of its admins."
		}
	}
	return 0, ""
}
