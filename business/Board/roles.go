package business

import (
	"slices"
	"strings"

	adapter "github.com/akashc777/OneCamp/adapter/Board"
	"github.com/akashc777/OneCamp/business/Access/level"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A board's roles (miro-32, the lead's rulings R2, R3 and R5, after Miro's):
//   - its owner (its maker, board_created_by) may do anything with it;
//   - a co-owner (board_owning_users, an editor too) shares it, changes its
//     settings and the editors-may-share switch, and gives and takes the roles
//     below co-owner, but never deletes it, never makes or removes co-owners
//     and never touches the owner's access;
//   - an editor draws, and shares only while the switch is on: invites, by
//     the one invite rule docs answer to (level.InviteRefusal), and guest links;
//   - everyone else who may read it reads it.
// Every check is the server's: each route asks these, never a copy of them.

// Refusal is a board role rule's refusal, in the words the route answers 403
// with.
type Refusal string

func (r Refusal) Error() string { return string(r) }

// The refusals.
const (
	ErrNotManager         Refusal = "Only the board's owner and co-owners can change that."
	ErrNotEditor          Refusal = "You need edit access to change this board."
	ErrNotDeleter         Refusal = "Only the board's owner can delete this board."
	ErrEditorsMayNotShare Refusal = "The board's owner has turned off sharing by editors."
	ErrOwnersAccess       Refusal = "No one can change the access of the board's owner."
	ErrCoOwnersOwnerOnly  Refusal = "Only the board's owner can make or remove co-owners."
	ErrInviteOnly         Refusal = "As an editor you can add people, not change or remove anyone's access."
	ErrAboveYourLevel     Refusal = "You can't give anyone more access than you have."
	ErrAlreadyOnBoard     Refusal = "That person already has access to this board."
)

// IsOwner is whether the person is the board's owner, its maker. Pure.
func IsOwner(board *dgraphStruct.DgraphBoard, userUUID string) bool {
	return board != nil && board.CreatedBy != nil && board.CreatedBy.Uuid != "" && board.CreatedBy.Uuid == userUUID
}

// CanManage is who may share a board, change its settings (its general
// access, the editors-may-share switch), give and take the roles below
// co-owner and protect its objects: its owner and co-owners; nobody, once it
// is deleted. Pure.
func CanManage(board *dgraphStruct.DgraphBoard, userUUID string) bool {
	return board != nil && !isDeleted(board) && (IsOwner(board, userUUID) || board.HasOwnerAccess > 0)
}

// CanDelete is who may delete a board (to the trash): its owner alone, and
// only while it is live. Pure.
func CanDelete(board *dgraphStruct.DgraphBoard, userUUID string) bool {
	return board != nil && !isDeleted(board) && IsOwner(board, userUUID)
}

// EditorsShare is the board's editors-may-share switch: on unless it was
// turned off. Pure.
func EditorsShare(board *dgraphStruct.DgraphBoard) bool {
	return board != nil && (board.EditorsShare == nil || *board.EditorsShare)
}

// MayShare is who may share a board, inviting people (an editor only adds,
// roleChangeRefusal): its owner and co-owners, and its editors while the
// switch is on. Its guest links are CanManage's alone (miro-33). Pure.
func MayShare(board *dgraphStruct.DgraphBoard, userUUID string) bool {
	return CanManage(board, userUUID) || (CanEdit(board, userUUID) && EditorsShare(board))
}

// roleChangeRefusal is why the person may not make the change to who holds
// which role, nil when they may. holders is each holder's level now by lower
// case uuid ("full" a co-owner, then "edit", "comment", "view"). The change is
// read as asked, before the one-role-each rule adds its removals. Pure.
func roleChangeRefusal(board *dgraphStruct.DgraphBoard, userUUID string, holders map[string]string, in adapter.InputUpdateBoardPermissions) error {
	if !CanManage(board, userUUID) && !CanEdit(board, userUUID) {
		return ErrNotManager
	}
	adds := map[level.Level][]string{level.Edit: in.AddEditors, level.Comment: in.AddCommenters, level.View: in.AddViewers, level.Full: in.AddCoOwners}
	removes := [][]string{in.RemoveEditors, in.RemoveCommenters, in.RemoveViewers, in.RemoveCoOwners}
	named := []string{}
	for _, ids := range adds {
		named = append(named, ids...)
	}
	for _, ids := range removes {
		named = append(named, ids...)
	}
	owner := IsOwner(board, userUUID)
	for _, id := range named {
		switch {
		case board.CreatedBy != nil && strings.EqualFold(id, board.CreatedBy.Uuid):
			return ErrOwnersAccess
		case !owner && holders[strings.ToLower(id)] == "full":
			return ErrCoOwnersOwnerOnly
		}
	}
	if !owner && len(in.AddCoOwners)+len(in.RemoveCoOwners) > 0 {
		return ErrCoOwnersOwnerOnly
	}
	if CanManage(board, userUUID) {
		return nil
	}
	if !EditorsShare(board) {
		return ErrEditorsMayNotShare
	}
	onlyAdds := true
	for _, ids := range removes {
		onlyAdds = onlyAdds && len(ids) == 0
	}
	var given []level.Level
	for l, ids := range adds {
		for range ids {
			given = append(given, l)
		}
	}
	switch level.InviteRefusal(level.Edit, onlyAdds, given) {
	case level.InviteForbidden:
		return ErrNotManager
	case level.InviteNotAnInvite, level.InviteNoLevel:
		return ErrInviteOnly
	case level.InviteAboveYourLevel:
		return ErrAboveYourLevel
	}
	for _, ids := range adds {
		for _, id := range ids {
			if holders[strings.ToLower(id)] != "" || strings.EqualFold(id, userUUID) {
				return ErrAlreadyOnBoard
			}
		}
	}
	return nil
}

// SettingsRefusal is why the person may not make the change to a board's
// settings, nil when they may: its general access and the editors-may-share
// switch are its owner's and co-owners' (the lead's R2), its title and
// thumbnail anyone's who may edit it. Pure.
func SettingsRefusal(board *dgraphStruct.DgraphBoard, userUUID string, in adapter.InputUpdateBoard) error {
	if (in.IsPrivate != nil || in.EditorsShare != nil) && !CanManage(board, userUUID) {
		return ErrNotManager
	}
	if !CanEdit(board, userUUID) {
		return ErrNotEditor
	}
	return nil
}

// oneRoleEach rewrites a role change so each person holds one role, a
// co-owner being an editor too (asked in, writes out): made a co-owner,
// they're given edit; taken off edit, or given a lower role, they stop
// being one; no longer a co-owner, they stay an editor unless the same
// change takes their edit away or gives them another role.
func oneRoleEach(asked adapter.InputUpdateBoardPermissions) adapter.InputUpdateBoardPermissions {
	input := asked
	input.RemoveCoOwners = slices.Concat(asked.RemoveCoOwners, asked.AddEditors, asked.AddViewers, asked.AddCommenters, asked.RemoveEditors)
	// No longer a co-owner, they stay an editor, unless the same change takes
	// their edit away or gives them another role (CR-270 F1).
	stay := slices.DeleteFunc(slices.Clone(asked.RemoveCoOwners), func(id string) bool {
		return slices.ContainsFunc(slices.Concat(asked.RemoveEditors, asked.AddViewers, asked.AddCommenters), func(s string) bool { return strings.EqualFold(s, id) })
	})
	input.AddEditors = slices.Concat(asked.AddEditors, asked.AddCoOwners, stay)
	input.RemoveEditors, input.RemoveCommenters, input.RemoveViewers = level.OneRoleEach(input.AddEditors, input.AddCommenters, input.AddViewers,
		input.RemoveEditors, input.RemoveCommenters, input.RemoveViewers)
	input.RemoveCoOwners = level.Unique(input.RemoveCoOwners)
	return input
}
