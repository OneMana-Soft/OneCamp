package business

import (
	"net/http"

	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// RequestSubject is the person a request is from, and who the Access Resolver
// decides for: the signed-in member the auth middleware put on its context
// (I11: never anything the client sent). With nobody signed in, both are
// empty, and a Subject with no UserUUID is no one. Admin is the account's
// own, for the few things it allows (Subject.ActingAdmin). Every handler that
// asks the resolver names its person this way.
func RequestSubject(r *http.Request) (userModels.UserInfo, Subject) {
	u, ok := userModels.FromContext(r.Context())
	if !ok || u == nil {
		return userModels.UserInfo{}, Subject{}
	}
	return *u, SubjectOf(u)
}

// SubjectOf is the person a member's account names, for the Access Resolver:
// RequestSubject's rule, for a caller that holds the account rather than the
// request (the collaboration service asking about a socket, a job, an agent
// acting for whoever asked it, which then sets AgentUUID). A nil account is
// no one.
func SubjectOf(u *userModels.UserInfo) Subject {
	if u == nil {
		return Subject{}
	}
	return Subject{UserUID: u.UserDgraphInfo.Uid, UserUUID: u.UserDgraphInfo.Uuid, Admin: u.UserPostgresInfo.IsAdmin}
}

// TokenSubjectOf is the person an API token acts for, for the Access
// Resolver: its owner's account by SubjectOf, with the agent the token is
// bound to (agentUUID, "" for none), and never the owner's admin powers. A
// token is least privilege (the owner's decision, 16 Oct 2026): what an admin
// may do only as themselves, in the app (recover a doc, delete another's for
// good, take a page into a space), no token of theirs may. A nil account is
// no one.
func TokenSubjectOf(u *userModels.UserInfo, agentUUID string) Subject {
	if u == nil {
		return Subject{}
	}
	s := SubjectOf(u)
	s.Admin = false
	s.AgentUUID = agentUUID
	return s
}
