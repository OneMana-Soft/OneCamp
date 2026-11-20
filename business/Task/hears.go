package business

// Who hears news of a task given to someone: ONE check, for every notice of a
// task's news to its assignee alone (urgent.go's Urgent, business/SLA's
// alerts, linear-24 H2). A second copy of it is a defect: a rule added here
// holds for every such notice.

import (
	"context"
	"slices"
	"strings"

	domain "github.com/akashc777/OneCamp/domain/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// Guests is whether a guest given the task hears the news: of the task itself,
// which they read (it was made Urgent), yes; of what only the project's
// members read (its SLA state), never.
type Guests bool

const (
	GuestsHear  Guests = true
	MembersOnly Guests = false
)

// hearSeams are who may read a task as its project's people and who reads as
// a guest: seams for tests.
var hearSeams = struct {
	readers func(ctx context.Context, taskUUID string, userUUIDs []string) (map[string]bool, error)
	guest   func(ctx context.Context, who string) bool
}{
	readers: domain.MemberReaders,
	guest:   userDomain.ReadsAsGuest,
}

// AssigneeHears is the task's assignee, when they are to hear news of it: never
// whoever made it (actorUUID; "" for the server's own, a sweep), while their
// setting for the project hears (everything or mentions: news of their own
// work is for them) and they may read the task as one of the project's people
// (domain MemberReaders); with MembersOnly, never a guest
// (userDomain.ReadsAsGuest, the rule SLA state is read by). Unread, nobody
// hears. The task needs its uuid, its project's uuid and its assignee's.
func AssigneeHears(ctx context.Context, t *dgraphStruct.DgraphTask, actorUUID string, guests Guests) (string, bool) {
	if t == nil || t.Uuid == "" || t.Project == nil || t.Project.Uuid == "" {
		return "", false
	}
	who := userUUIDOf(t.Assignee)
	if who == "" || strings.EqualFold(who, actorUUID) {
		return "", false
	}
	// Nobody made it: nobody is left out as its maker (a setting's query
	// compares ids, so "" would fail it).
	exclude := actorUUID
	if exclude == "" {
		exclude = uuid.Nil.String()
	}
	hear, err := hearersBy(ctx, hearSeams.readers, t.Project.Uuid, t.Uuid, exclude, []string{who}, true)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Task AssigneeHears: who hears of task %s couldn't be read, telling nobody: %+v", t.Uuid, err)
		return "", false
	}
	if !slices.Contains(hear, who) || (guests == MembersOnly && hearSeams.guest(ctx, who)) {
		return "", false
	}
	return who, true
}
