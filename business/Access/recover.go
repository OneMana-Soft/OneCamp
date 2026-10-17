package business

// Admin recovery and the lock.
//
// When someone leaves, a workspace admin can take over a resource they owned,
// or hand it to someone else. It is loud: recorded here and in the admin audit
// log (the controller writes that), told to the events bus with the owner
// before named, and shown to the resource's owner and full-access people in
// its sharing panel.
//
// A lock is soft or hard (D-12). A soft lock is the editor's guard, which
// anyone who may edit can set or lift. A hard lock holds everyone below full at
// comment, on the server and on every live socket; only the owner or someone
// with full access sets or lifts it.

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/akashc777/OneCamp/business/Access/level"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	accessModels "github.com/akashc777/OneCamp/models/postgres/Access"
	"github.com/google/uuid"
)

// Recover has an admin take over a resource (to themselves, or to toUserUUID
// when it is set), whatever its sharing and whether or not it is in the
// trash. The owner before keeps full access.
func Recover(ctx context.Context, admin Subject, res Resource, toUserUUID, reason string) (*accessModels.Recovery, error) {
	if !admin.ActingAdmin() {
		return nil, ErrForbidden
	}
	if res.Kind != KindDoc {
		return nil, ErrUnknownKind
	}
	resID, err := uuid.Parse(res.UUID)
	if err != nil {
		return nil, ErrNotFound
	}
	adminID, err := uuid.Parse(admin.UserUUID)
	if err != nil {
		return nil, ErrForbidden
	}
	to := admin.UserUUID
	if toUserUUID != "" {
		to = toUserUUID
	}
	toID, err := uuid.Parse(to)
	if err != nil {
		return nil, ErrNoSuchPrincipal
	}
	reason = strings.TrimSpace(reason)
	if utf8.RuneCountInString(reason) > maxNote {
		reason = string([]rune(reason)[:maxNote])
	}
	from, err := handOn(ctx, res, toID.String(), admin, resourceEventBusiness.DocRecovered,
		map[string]string{"admin": strings.ToLower(admin.UserUUID), "reason": reason})
	if err != nil {
		return nil, err
	}
	rec := accessModels.Recovery{
		ID: uuid.New(), ResourceKind: res.Kind, ResourceUUID: resID, AdminUUID: adminID,
		ToOwnerUUID: toID, Reason: reason, CreatedAt: now(),
	}
	if f, err := uuid.Parse(from); err == nil {
		rec.FromOwnerUUID = &f
	}
	if err := accessModels.RecordRecovery(ctx, rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// Lock sets a resource's lock: level.LockSoft, level.LockHard, or
// level.LockNone to lift it. Answers who's decision after it.
func Lock(ctx context.Context, who Subject, res Resource, lock string) (Decision, error) {
	switch lock {
	case level.LockNone, level.LockSoft, level.LockHard:
	default:
		return Decision{}, ErrBadLevel
	}
	d, err := Rules.Resolve(ctx, who, res)
	if err != nil {
		return Decision{}, err
	}
	switch {
	case d.Deleted:
		return Decision{}, docBusiness.ErrDocDeleted
	case d.Level == LevelNone:
		return Decision{}, ErrNotFound
	// A hard lock, set or lifted, is full access's; so is anything done to
	// a doc that is hard-locked now.
	case (lock == level.LockHard || d.Locked) && d.Level < LevelFull:
		return Decision{}, ErrForbidden
	// A soft lock is anyone's who may edit.
	case d.Level < LevelEdit:
		return Decision{}, ErrForbidden
	}
	if d.Lock == lock {
		return d, nil
	}
	if err := docBusiness.SetLock(ctx, res.UUID, lock, who.UserUUID, now()); err != nil {
		return Decision{}, err
	}
	// A hard lock moves every editor's level, one way or the other: every
	// socket on the doc is asked again. A soft lock moves nobody's.
	ex := docBusiness.Explained{}
	if lock == level.LockHard || d.Lock == level.LockHard {
		ex.Everyone = &docBusiness.Shift{}
	}
	docBusiness.AccessChanged(ctx, []string{res.UUID}, docBusiness.Actor{UserUUID: who.UserUUID, AgentUUID: who.AgentUUID},
		resourceEventBusiness.DocLocked, ex, map[string]string{"lock": lock, "was": d.Lock})
	return Rules.Resolve(ctx, who, res)
}
