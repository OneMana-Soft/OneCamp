package business

// Money is for project admins only (V-698): a money field is listed to
// everyone who views its project, but its values are read and set only by
// the project's admins, the workAccess NeedManage check insights' money
// slices made first. MoneyProjects is that one check; every reader of task
// field values goes through it: MergeFieldValues (the task list, the board,
// the task panel, the CSV, the report), Hidden (the field lists and the
// CSV's columns), FilterClause, MaySet (setting a value, a template's typed
// values) and ForEveryone (the live messages and the Work Change Log, which
// every member hears alike, so they carry no money at all).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/TaskField"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/taskread"
)

// ErrMoneyAdminsOnly refuses a money value from anyone not an admin of its
// project; MoneyAdminsOnly says so in words.
var ErrMoneyAdminsOnly = errors.New("only project admins can set money fields")

// MoneyAdminsOnly is ErrMoneyAdminsOnly as the person is told it.
const MoneyAdminsOnly = "Only project admins can set money fields."

// Reader is whose reads and writes of field values these are: a person, by
// their id and graph uid (either may be empty, not both), or the server
// itself, which reads every value. The zero Reader is nobody, an admin of
// nothing.
type Reader struct {
	UUID, UID string
	System    bool
}

// ReaderOf is ctx's reader as models/taskread names it: the server for a
// context marked taskread.WithSystem, else the person it acts for or the
// request's signed-in person (a token's call is its owner's), else nobody,
// so a background caller with a bare context sees no money (fails closed).
func ReaderOf(ctx context.Context) Reader {
	if taskread.System(ctx) {
		return Reader{System: true}
	}
	r := Reader{UUID: taskread.ReaderOf(ctx)}
	if r.UUID == "" {
		return r
	}
	var u *userModels.UserInfo
	switch v := ctx.Value(helpers.UserInfoContextKey).(type) {
	case userModels.UserInfo:
		u = &v
	case *userModels.UserInfo:
		u = v
	}
	if u != nil && (strings.EqualFold(u.UserPostgresInfo.Id.String(), r.UUID) || strings.EqualFold(u.UserDgraphInfo.Uuid, r.UUID)) {
		r.UID = u.UserDgraphInfo.Uid
	}
	return r
}

// projectsWhere is workAccess.ProjectsWhere; a var so tests can stand in for
// the graph.
var projectsWhere = workAccess.ProjectsWhere

// MoneyProjects is which of projects (ids) r may read and set money values
// in, by lower-case id: those r is an admin of (NeedManage). The server may
// in all of them, nobody in none. A failed read is returned with none, so a
// caller that logs it and goes on keeps money from everyone.
func MoneyProjects(ctx context.Context, r Reader, projects []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(projects) == 0 {
		return out, nil
	}
	if r.System {
		for _, p := range projects {
			out[strings.ToLower(p)] = true
		}
		return out, nil
	}
	if r.UUID == "" && r.UID == "" {
		return out, nil
	}
	admin, err := projectsWhere(ctx, r.UUID, r.UID, workAccess.NeedManage, projects)
	if err != nil {
		return out, err
	}
	return admin, nil
}

// hiddenOf is which of the money fields given (field id, then its project's
// id) r may not read: those of projects r isn't an admin of, every one of
// them when the read fails.
func hiddenOf(ctx context.Context, r Reader, money map[string]string) map[string]bool {
	hidden := map[string]bool{}
	if len(money) == 0 {
		return hidden
	}
	seen := map[string]bool{}
	var projects []string
	for _, p := range money {
		if !seen[p] {
			seen[p] = true
			projects = append(projects, p)
		}
	}
	admin, err := MoneyProjects(ctx, r, projects)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/TaskField money admins err: %+v", err)
	}
	for f, p := range money {
		if !admin[strings.ToLower(p)] {
			hidden[f] = true
		}
	}
	return hidden
}

// moneyOf is the fields among fields whose values are money's to keep:
// field id, then its project's id. The money fields, and every formula that
// reads one, directly or through other formulas (J-F, tainted): its value
// would tell of the money it reads.
func moneyOf(fields []*model.Field) map[string]string {
	return tainted(fields)
}

// MoneyOf is moneyOf for a reader that has the admin set MoneyProjects
// gives already (the report): field id, then its project's id. Pure.
func MoneyOf(fields []*model.Field) map[string]string { return moneyOf(fields) }

// Hidden is which of fields (by id) keep their values from r: the money
// fields of the projects r isn't an admin of, and the formulas reading them
// (moneyOf). One read, and only when a money field is among them.
func Hidden(ctx context.Context, r Reader, fields []*model.Field) map[string]bool {
	return hiddenOf(ctx, r, moneyOf(fields))
}

// Readable is fields less those Hidden keeps from r, in order.
func Readable(ctx context.Context, r Reader, fields []*model.Field) []*model.Field {
	hidden := Hidden(ctx, r, fields)
	out := make([]*model.Field, 0, len(fields))
	for _, f := range fields {
		if f != nil && !hidden[f.ID.String()] {
			out = append(out, f)
		}
	}
	return out
}

// MaySet says whether r may set values of f, for a caller that has already
// checked r may edit the task: any field's, but a money field's only as an
// admin of its project (ErrMoneyAdminsOnly, also when that can't be read).
func MaySet(ctx context.Context, r Reader, f *model.Field) error {
	if f == nil || f.Type != TypeMoney {
		return nil
	}
	if hiddenOf(ctx, r, moneyOf([]*model.Field{f}))[f.ID.String()] {
		return ErrMoneyAdminsOnly
	}
	return nil
}

// ForEveryone is what a message every member of f's project hears alike (a
// live message, a Work Change Log entry) may carry of a value of f: a money
// field's value is no one's to hear that way, so it carries none and asks
// each app to read the task again as itself (refetch). Pure.
func ForEveryone(f *model.Field, value json.RawMessage) (told json.RawMessage, refetch bool) {
	if f != nil && f.Type == TypeMoney {
		return nil, true
	}
	return value, false
}

// blank takes the values hidden names off each task's values. Pure.
func blank(values model.Values, hidden map[string]bool) {
	if len(hidden) == 0 {
		return
	}
	for _, fields := range values {
		for id := range fields {
			if hidden[id] {
				delete(fields, id)
			}
		}
	}
}
