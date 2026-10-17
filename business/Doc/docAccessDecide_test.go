package businness

import (
	"slices"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/business/Access/level"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// The access rule over role × source × modifier: every way a person reaches a
// doc, each alone, then with a hard lock, a soft lock and in the trash. A row
// here is what a read of the doc for the person carries (domain/Doc
// docAccess.go's facts).
func TestTheAccessRuleOverSourcesAndModifiers(t *testing.T) {
	yes, no := true, false
	me := Viewer{UID: "0x2", UUID: "me-uuid"}
	mine := &dgraphStruct.DgraphUser{Uid: me.UID, Uuid: me.UUID}
	other := &dgraphStruct.DgraphUser{Uid: "0x9", Uuid: "other-uuid"}
	private := func(edit func(*dgraphStruct.DgraphDoc)) *dgraphStruct.DgraphDoc {
		d := &dgraphStruct.DgraphDoc{IsPrivate: &yes, CreatedBy: other}
		edit(d)
		return d
	}

	sources := []struct {
		name string
		doc  *dgraphStruct.DgraphDoc
		want level.Level
		why  level.Reason
	}{
		{"nothing: a private doc of someone else's", private(func(*dgraphStruct.DgraphDoc) {}), level.None, ""},
		{"its maker, before owners were recorded", private(func(d *dgraphStruct.DgraphDoc) { d.CreatedBy = mine }), level.Full, level.Owner},
		{"its owner, handed it by its maker", private(func(d *dgraphStruct.DgraphDoc) { d.Owner = mine }), level.Full, level.Owner},
		{"full access", private(func(d *dgraphStruct.DgraphDoc) { d.HasFullAccess = 1 }), level.Full, level.FullAccess},
		{"shared to edit", private(func(d *dgraphStruct.DgraphDoc) { d.HasEditAccess = 1 }), level.Edit, level.SharedEdit},
		{"shared to comment", private(func(d *dgraphStruct.DgraphDoc) { d.HasCommentAccess = 1 }), level.Comment, level.SharedComment},
		{"shared to view", private(func(d *dgraphStruct.DgraphDoc) { d.HasReadAccess = 1 }), level.View, level.SharedView},
		{"a team they're in can edit", private(func(d *dgraphStruct.DgraphDoc) { d.HasTeamEditAccess = 1 }), level.Edit, level.TeamEdit},
		{"a team they're in can comment", private(func(d *dgraphStruct.DgraphDoc) { d.HasTeamCommentAccess = 1 }), level.Comment, level.TeamComment},
		{"a team they're in can view", private(func(d *dgraphStruct.DgraphDoc) { d.HasTeamViewAccess = 1 }), level.View, level.TeamView},
		{"its project's space: at least view", private(func(d *dgraphStruct.DgraphDoc) { d.InProjectSpace = "p1" }), level.View, level.Space},
		{"its team's space, which may edit", private(func(d *dgraphStruct.DgraphDoc) { d.InTeamSpace = "t1"; d.PublicEdit = &yes }), level.Edit, level.Space},
		{"everyone in the workspace can view", &dgraphStruct.DgraphDoc{IsPrivate: &no, CreatedBy: other}, level.View, level.Workspace},
		{"privacy never recorded: the workspace's", &dgraphStruct.DgraphDoc{CreatedBy: other}, level.View, level.Workspace},
		{"everyone can comment", &dgraphStruct.DgraphDoc{IsPrivate: &no, PublicComment: &yes, CreatedBy: other}, level.Comment, level.Workspace},
		{"everyone can edit", &dgraphStruct.DgraphDoc{IsPrivate: &no, PublicEdit: &yes, CreatedBy: other}, level.Edit, level.Workspace},
		{"a reader of a doc every reader may comment on", private(func(d *dgraphStruct.DgraphDoc) { d.HasReadAccess = 1; d.PublicComment = &yes }), level.Comment, level.ReadersComment},
		{"everyone can edit, but it's private: nobody but its space", private(func(d *dgraphStruct.DgraphDoc) { d.PublicEdit = &yes }), level.None, ""},
		{"the strongest source wins", private(func(d *dgraphStruct.DgraphDoc) { d.HasReadAccess = 1; d.HasTeamEditAccess = 1 }), level.Edit, level.TeamEdit},
	}

	gone := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, s := range sources {
		t.Run(s.name, func(t *testing.T) {
			d := DecideDoc(s.doc, me)
			if d.Level != s.want || d.Was != s.want || d.Unlocked != s.want {
				t.Fatalf("level %s (was %s, unlocked %s), want %s", d.Level, d.Was, d.Unlocked, s.want)
			}
			if s.want == level.None {
				if len(d.Reasons) != 0 {
					t.Fatalf("a level of none gives no reasons, got %v", d.Reasons)
				}
			} else if !slices.Contains(d.Reasons, s.why) {
				t.Fatalf("reasons %v, want %s among them", d.Reasons, s.why)
			}

			// A hard lock holds everyone below full at comment.
			locked := *s.doc
			locked.LockLevel = level.LockHard
			ld := DecideDoc(&locked, me)
			want := s.want
			if want > level.Comment && want < level.Full {
				want = level.Comment
			}
			if ld.Level != want || !ld.Locked() || ld.Unlocked != s.want {
				t.Fatalf("hard-locked: level %s unlocked %s locked %v, want %s, %s, true", ld.Level, ld.Unlocked, ld.Locked(), want, s.want)
			}
			if want < s.want && !slices.Contains(ld.Reasons, level.HardLock) {
				t.Fatalf("a level the lock held says so: %v", ld.Reasons)
			}

			// A soft lock is the editor's guard and changes nobody's level.
			soft := *s.doc
			soft.LockLevel = level.LockSoft
			if sd := DecideDoc(&soft, me); sd.Level != s.want || sd.Locked() || sd.Lock != level.LockSoft {
				t.Fatalf("soft-locked: level %s locked %v lock %q", sd.Level, sd.Locked(), sd.Lock)
			}

			// In the trash: nobody, the owner included; the trash decides by Was.
			deleted := *s.doc
			deleted.DeletedAt = &gone
			dd := DecideDoc(&deleted, me)
			if dd.Level != level.None || !dd.Deleted || dd.Was != s.want || len(dd.Reasons) != 0 {
				t.Fatalf("deleted: level %s deleted %v was %s reasons %v, want none, true, %s", dd.Level, dd.Deleted, dd.Was, dd.Reasons, s.want)
			}
			if dd.Owner != d.Owner {
				t.Fatal("a deleted doc stays its owner's")
			}
			// Archived by the workspace's retention: deleted, and says so.
			archived := deleted
			archived.ArchivedAt = &gone
			if ad := DecideDoc(&archived, me); !ad.Archived || ad.Level != level.None || ad.Was != s.want {
				t.Fatalf("archived: %+v", ad)
			}

			// The yes-or-no checks answer from the same rule.
			if CanRead(s.doc, me.UUID) != (s.want >= level.View) ||
				CanComment(s.doc, me.UUID) != (s.want >= level.Comment) ||
				CanEdit(s.doc, me.UID) != (s.want >= level.Edit) ||
				CouldRead(&deleted, me.UUID) != (s.want >= level.View) ||
				CouldEdit(&deleted, me.UID) != (s.want >= level.Edit) ||
				CanRead(&deleted, me.UUID) || CanEdit(&deleted, me.UID) {
				t.Fatal("a yes-or-no check disagrees with the rule")
			}
		})
	}
}

// The owner is matched by uid or uuid, whichever the read and the person have,
// and an unknown never matches an unknown.
func TestTheOwnerIsMatchedByWhatBothSidesHave(t *testing.T) {
	yes := true
	doc := func(owner *dgraphStruct.DgraphUser) *dgraphStruct.DgraphDoc {
		return &dgraphStruct.DgraphDoc{IsPrivate: &yes, Owner: owner}
	}
	for _, c := range []struct {
		name  string
		owner *dgraphStruct.DgraphUser
		v     Viewer
		want  bool
	}{
		{"by uid", &dgraphStruct.DgraphUser{Uid: "0x2", Uuid: "a"}, Viewer{UID: "0x2"}, true},
		{"by uuid, any case", &dgraphStruct.DgraphUser{Uid: "0x2", Uuid: "AbC"}, Viewer{UUID: "abc"}, true},
		{"a uid that differs, whatever the uuid", &dgraphStruct.DgraphUser{Uid: "0x3", Uuid: "a"}, Viewer{UID: "0x2", UUID: "a"}, false},
		{"by uuid when the read has no uid", &dgraphStruct.DgraphUser{Uuid: "a"}, Viewer{UID: "0x2", UUID: "a"}, true},
		{"an empty uuid never matches an empty one", &dgraphStruct.DgraphUser{}, Viewer{}, false},
		{"no owner and no maker", nil, Viewer{UID: "0x2", UUID: "a"}, false},
	} {
		if got := DecideDoc(doc(c.owner), c.v).Owner; got != c.want {
			t.Errorf("%s: owner %v, want %v", c.name, got, c.want)
		}
	}
	// IsOwner, which shares and hands a doc on, is the same rule.
	if !IsOwner(doc(&dgraphStruct.DgraphUser{Uid: "0x2"}), "0x2") || IsOwner(doc(&dgraphStruct.DgraphUser{Uid: "0x2"}), "") {
		t.Error("IsOwner disagrees with the rule")
	}
	// A handed-on doc is its new owner's, not its maker's.
	handed := &dgraphStruct.DgraphDoc{IsPrivate: &yes, CreatedBy: &dgraphStruct.DgraphUser{Uid: "0x2"}, Owner: &dgraphStruct.DgraphUser{Uid: "0x3"}}
	if IsOwner(handed, "0x2") || !IsOwner(handed, "0x3") || CanRead(handed, "") {
		t.Error("the maker of a doc handed on still owns it")
	}
}

// The counts the web app decides by say what the rule says: a team's editor
// may edit, an editor a hard lock holds may not.
func TestTheAppsCountsSayWhatTheRuleSays(t *testing.T) {
	yes := true
	team := &dgraphStruct.DgraphDoc{IsPrivate: &yes, HasTeamEditAccess: 1}
	showDecision(team, DecideDoc(team, Viewer{UUID: "me"}))
	if team.HasEditAccess != 1 || team.HasCommentAccess != 1 || team.HasReadAccess != 1 || team.HasFullAccess != 0 {
		t.Errorf("a team's editor: %+v", team)
	}
	locked := &dgraphStruct.DgraphDoc{IsPrivate: &yes, HasEditAccess: 3, LockLevel: level.LockHard}
	showDecision(locked, DecideDoc(locked, Viewer{UUID: "me"}))
	if locked.HasEditAccess != 0 || locked.HasCommentAccess != 1 {
		t.Errorf("an editor a hard lock holds: %+v", locked)
	}
}

// Who may restore a deleted doc, and delete it for good: one rule for the
// trash and the doc page's 410 (A0's can_restore), by the level before the
// deletion. Full access restores, as the owner and editors do; a commenter
// doesn't; an admin always may.
func TestWhoMayRestoreAndPurgeADeletedDoc(t *testing.T) {
	yes := true
	gone := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	me := Viewer{UID: "0x2", UUID: "me"}
	doc := func(edit func(*dgraphStruct.DgraphDoc)) *dgraphStruct.DgraphDoc {
		d := &dgraphStruct.DgraphDoc{IsPrivate: &yes, DeletedAt: &gone, CreatedBy: &dgraphStruct.DgraphUser{Uid: "0x9"}}
		edit(d)
		return d
	}
	for _, c := range []struct {
		name           string
		doc            *dgraphStruct.DgraphDoc
		admin          bool
		restore, purge bool
	}{
		{"its owner", doc(func(d *dgraphStruct.DgraphDoc) { d.Owner = &dgraphStruct.DgraphUser{Uid: "0x2"} }), false, true, true},
		{"full access", doc(func(d *dgraphStruct.DgraphDoc) { d.HasFullAccess = 1 }), false, true, true},
		{"an editor", doc(func(d *dgraphStruct.DgraphDoc) { d.HasEditAccess = 1 }), false, true, false},
		{"a team's editor", doc(func(d *dgraphStruct.DgraphDoc) { d.HasTeamEditAccess = 1 }), false, true, false},
		{"a commenter", doc(func(d *dgraphStruct.DgraphDoc) { d.HasCommentAccess = 1 }), false, false, false},
		{"an editor of a doc locked hard", doc(func(d *dgraphStruct.DgraphDoc) { d.HasEditAccess = 1; d.LockLevel = level.LockHard }), false, false, false},
		{"an admin with nothing", doc(func(*dgraphStruct.DgraphDoc) {}), true, true, true},
		{"someone with nothing", doc(func(*dgraphStruct.DgraphDoc) {}), false, false, false},
	} {
		if got := CanRestore(c.doc, me, c.admin); got != c.restore {
			t.Errorf("%s restores: %v, want %v", c.name, got, c.restore)
		}
		if got := CanPurge(c.doc, me, c.admin); got != c.purge {
			t.Errorf("%s purges: %v, want %v", c.name, got, c.purge)
		}
		// The trash's own checks are the same rule.
		if !c.admin && couldEditDeleted(c.doc, me.UID) != c.restore {
			t.Errorf("%s: the trash disagrees", c.name)
		}
	}
}
