package domain

// How much someone may do with a doc, decided from what a read of it for them
// carries (docAccess.go's facts): the one rule every doc check uses, from
// opening a doc to joining it live (invariant I9). It is here, below
// business/Doc, so that the packages business/Doc depends on ask the same rule
// (business/Archive's restore list, through CouldRead); business/Doc and the
// Access Resolver (business/Access) ask it through business/Doc.
//
// The sources, strongest first:
//   - the owner, and the people given full access: full;
//   - the people it is shared with, at the level they were given;
//   - the teams it is shared with: their members and admins, at the team's
//     level;
//   - its space (a project's or a team's): that space's members and admins,
//     at its general level and never below view. One hop: a doc's own space,
//     never a space above it;
//   - its general access, when it isn't private: everyone in the workspace,
//     at view, comment or edit;
//   - "everyone who can read it may comment" raises a reader to comment.
//
// The modifiers: a hard lock holds everyone below full at comment (D-12), and
// a deleted doc is open to nobody outside the trash, which decides by the
// level the person had before it was deleted (Was).

import (
	"strings"

	"github.com/akashc777/OneCamp/business/Access/level"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Viewer is who a decision is for: their graph uid, their uuid, or both. A
// doc read for them carries their own facts already; the ids say which owner
// is theirs.
type Viewer struct {
	UID  string
	UUID string
}

// DocDecision is someone's level on a doc, and why.
type DocDecision struct {
	// Level is what they may do now: LevelNone for a deleted doc, and held at
	// comment by a hard lock.
	Level level.Level
	// Unlocked is the level a hard lock holds them below; Level when it doesn't.
	Unlocked level.Level
	// Was is their level before the doc was deleted (the trash and the archive
	// decide by it); Level for a live doc.
	Was      level.Level
	Owner    bool
	Deleted  bool
	Archived bool
	// Hosted is a doc that belongs to something else (D-18, docHost.go): the
	// rule gives it none, and business/Doc asks its host on a path that
	// opens one.
	Hosted bool
	// NodeKind is the doc's kind when it isn't a page (docs F1,
	// docNodeKind.go), read with every decision (accessScalars), so the paths
	// only a page takes (the editor's session) refuse the rest without
	// another read. It decides no level.
	NodeKind string
	// Lock is the doc's lock: level.LockNone, LockSoft or LockHard.
	Lock    string
	Reasons []level.Reason
}

// Locked is whether a hard lock is on: everyone below full is held at comment.
func (d DocDecision) Locked() bool { return d.Lock == level.LockHard }

// isTheViewer says whether a person the doc names (its owner, its maker) is
// the viewer, by graph uid or by uuid, whichever the read and the viewer have.
func isTheViewer(u *dgraphStruct.DgraphUser, v Viewer) bool {
	if u == nil {
		return false
	}
	if v.UID != "" && u.Uid != "" {
		return u.Uid == v.UID
	}
	return v.UUID != "" && u.Uuid != "" && strings.EqualFold(u.Uuid, v.UUID)
}

// OwnerOf is who owns the doc: its owner, or for a doc made before owners were
// recorded, its maker.
func OwnerOf(doc *dgraphStruct.DgraphDoc) *dgraphStruct.DgraphUser {
	if doc.Owner != nil && (doc.Owner.Uid != "" || doc.Owner.Uuid != "") {
		return doc.Owner
	}
	return doc.CreatedBy
}

// IsPrivate is whether only the people it is shared with (and its space) may
// open it. A doc whose privacy wasn't recorded is the workspace's, as it
// always has been.
func IsPrivate(doc *dgraphStruct.DgraphDoc) bool {
	return doc.IsPrivate != nil && *doc.IsPrivate
}

// GeneralLevel is what the doc's general audience gets: everyone in the
// workspace when it isn't private, its space's members when it is in a space.
func GeneralLevel(doc *dgraphStruct.DgraphDoc) level.Level {
	switch {
	case doc == nil:
		return level.None
	case doc.PublicEdit != nil && *doc.PublicEdit:
		return level.Edit
	case doc.PublicComment != nil && *doc.PublicComment:
		return level.Comment
	}
	return level.View
}

// DecideDoc is the person's level on a doc read for them.
func DecideDoc(doc *dgraphStruct.DgraphDoc, v Viewer) DocDecision {
	var d DocDecision
	if doc == nil {
		return d
	}
	d.NodeKind = doc.NodeKind
	if IsHosted(doc) {
		// A hosted doc's level is its host's (D-18): business/Doc asks the
		// host on a path that opens one (docHost.go), and every other path,
		// which asks only this rule, finds it open to nobody, whoever made it.
		d.Hosted = true
		d.Deleted = helpers.IsSoftDeleted(doc.DeletedAt)
		d.Archived = helpers.IsSoftDeleted(doc.ArchivedAt)
		return d
	}
	d.Owner = isTheViewer(OwnerOf(doc), v)
	d.Deleted = helpers.IsSoftDeleted(doc.DeletedAt)
	d.Archived = helpers.IsSoftDeleted(doc.ArchivedAt)
	switch doc.LockLevel {
	case level.LockSoft, level.LockHard:
		d.Lock = doc.LockLevel
	}

	lvl := level.None
	var reasons []level.Reason
	grant := func(l level.Level, why level.Reason) {
		if l > lvl {
			lvl = l
		}
		reasons = append(reasons, why)
	}
	if d.Owner {
		grant(level.Full, level.Owner)
	}
	if doc.HasFullAccess > 0 {
		grant(level.Full, level.FullAccess)
	}
	if doc.HasEditAccess > 0 {
		grant(level.Edit, level.SharedEdit)
	}
	if doc.HasTeamEditAccess > 0 {
		grant(level.Edit, level.TeamEdit)
	}
	if doc.HasCommentAccess > 0 {
		grant(level.Comment, level.SharedComment)
	}
	if doc.HasTeamCommentAccess > 0 {
		grant(level.Comment, level.TeamComment)
	}
	if doc.HasReadAccess > 0 {
		grant(level.View, level.SharedView)
	}
	if doc.HasTeamViewAccess > 0 {
		grant(level.View, level.TeamView)
	}
	general := GeneralLevel(doc)
	if doc.InProjectSpace != "" || doc.InTeamSpace != "" {
		grant(max(level.View, general), level.Space)
	}
	if !IsPrivate(doc) {
		grant(general, level.Workspace)
	}
	if lvl == level.View && doc.PublicComment != nil && *doc.PublicComment {
		grant(level.Comment, level.ReadersComment)
	}

	d.Unlocked = lvl
	if d.Lock == level.LockHard && lvl > level.Comment && lvl < level.Full {
		lvl = level.Comment
		reasons = append(reasons, level.HardLock)
	}
	d.Was = lvl
	if d.Deleted || lvl == level.None {
		d.Level = level.None
		if d.Deleted {
			d.Unlocked = level.None
		}
		return d
	}
	d.Level, d.Reasons = lvl, reasons
	return d
}
