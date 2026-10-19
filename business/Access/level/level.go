// Package level is the Access Resolver's vocabulary: how much someone may do
// with a resource, and the reasons they may (docs programme, PROGRAM.md §4 #12).
//
// It is a package of its own so that a kind the resolver decides for
// (business/Doc) can speak it without importing business/Access, which
// imports that kind. business/Access re-exports every name here, so code that
// asks the resolver never needs this package.
package level

// Level is how much someone may do. The levels are ordered:
// none < view < comment < edit < full. Full may also share, lock hard and hand
// the resource on.
type Level int

const (
	None Level = iota
	View
	Comment
	Edit
	Full
)

// String is the level's name as the API and the web app spell it.
func (l Level) String() string {
	switch l {
	case View:
		return "view"
	case Comment:
		return "comment"
	case Edit:
		return "edit"
	case Full:
		return "full"
	}
	return "none"
}

// Named reads a level as the API spells it. ok is false for anything else.
func Named(s string) (l Level, ok bool) {
	switch s {
	case "none":
		return None, true
	case "view":
		return View, true
	case "comment":
		return Comment, true
	case "edit":
		return Edit, true
	case "full":
		return Full, true
	}
	return None, false
}

// Reason is where a level comes from, as a code the web app words for the
// reader ("You own this doc", "Design, a team you're in, can edit"). Codes are
// never renamed once shipped.
type Reason string

const (
	// Owner: they own it.
	Owner Reason = "owner"
	// FullAccess: they were given full access.
	FullAccess Reason = "full_access"
	// SharedEdit, SharedComment and SharedView: it is shared with them.
	SharedEdit    Reason = "shared_edit"
	SharedComment Reason = "shared_comment"
	SharedView    Reason = "shared_view"
	// TeamEdit, TeamComment and TeamView: it is shared with a team they're in.
	TeamEdit    Reason = "team_edit"
	TeamComment Reason = "team_comment"
	TeamView    Reason = "team_view"
	// Space: it is in the space of a project or team they're in (one hop: the
	// resource's own space, never one above it).
	Space Reason = "space"
	// Workspace: everyone in the workspace has it.
	Workspace Reason = "workspace"
	// ReadersComment: everyone who can read it may comment.
	ReadersComment Reason = "readers_comment"
	// HardLock: a hard lock holds them at comment.
	HardLock Reason = "hard_lock"
	// Host: it belongs to something else they may open (a table row's page,
	// docs D-18), and its level is that thing's, never above edit.
	Host Reason = "host"
)

// Lock levels (D-12). A soft lock is a guard in the editor, which anyone who
// may edit can lift; a hard lock is the server's, holds everyone below full at
// comment, and only the owner or someone with full access lifts it.
const (
	LockNone = ""
	LockSoft = "soft"
	LockHard = "hard"
)

// MarshalText writes a level by its name, so the API says "edit", never 3.
func (l Level) MarshalText() ([]byte, error) { return []byte(l.String()), nil }

// UnmarshalText reads a level by its name; anything else is an error.
func (l *Level) UnmarshalText(b []byte) error {
	v, ok := Named(string(b))
	if !ok {
		return errUnknownLevel(string(b))
	}
	*l = v
	return nil
}

type errUnknownLevel string

func (e errUnknownLevel) Error() string { return "no access level " + string(e) }
