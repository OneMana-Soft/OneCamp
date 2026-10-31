package models

// Members let into a call through a link, as themselves (migration
// *_call_admissions, calls HU13): business/Guest writes and reads them.

import (
	"context"
	"database/sql"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Admission is a member let into a call through a link (call_admissions):
// at a meeting's link or a room's address for as long as the link lives
// (Session nil), or through a call's invite for that one call.
type Admission struct {
	Room     string
	UserUUID uuid.UUID
	// Identity is their call identity (graph uid), to put them out.
	Identity string
	GrantID  uuid.UUID
	Session  *uuid.UUID
	At       time.Time
}

// Admit records an admission. The same member let in through the same link
// again keeps one row, the latest.
func Admit(ctx context.Context, a Admission) error {
	_, err := pgexec.Exec(ctx, `INSERT INTO call_admissions (room, user_uuid, identity, grant_id, session_id, admitted_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (room, user_uuid, grant_id) DO UPDATE
		SET identity = EXCLUDED.identity, session_id = EXCLUDED.session_id, admitted_at = EXCLUDED.admitted_at`,
		a.Room, a.UserUUID, a.Identity, a.GrantID, a.Session, a.At)
	return err
}

// LiveAdmission is a live link a member came into a call's room through.
type LiveAdmission struct {
	Room    string
	GrantID uuid.UUID
	// Standing is a personal room's address, which answers only while rooms are on.
	Standing bool
}

// LiveAdmissionsOf answers the links through which a member may be in a call
// now, by room: each link live under the workspace's link cap (capDays), and
// a call's invite only for the call going on in its room. One query.
func LiveAdmissionsOf(ctx context.Context, user uuid.UUID, capDays int) ([]LiveAdmission, error) {
	q := `SELECT a.room, a.grant_id, g.resource_type, g.slug
		FROM call_admissions a JOIN guest_grants g ON g.id = a.grant_id
		WHERE a.user_uuid = $1
		  AND a.grant_id IN (` + guestModel.LiveGrantsSQL(2) + `)
		  AND (a.session_id IS NULL OR a.session_id IN (SELECT s.id FROM call_sessions s WHERE s.room = a.room AND s.ended_at IS NULL))`
	return pgexec.Query(ctx, q, func(r pgexec.Row) (LiveAdmission, error) {
		var l LiveAdmission
		var g guestModel.GuestGrant
		var slug sql.NullString
		if err := r.Scan(&l.Room, &l.GrantID, &g.ResourceType, &slug); err != nil {
			return l, err
		}
		if slug.Valid {
			g.Slug = &slug.String
		}
		l.Standing = guestModel.IsStanding(&g)
		return l, nil
	}, user, capDays)
}

// LiveCallsOfPosts answers, for posts that are a call's post, the room of the
// call going on there (a huddle's thread is open to its invitees only while
// its call goes on). One query; posts with no live call are absent.
func LiveCallsOfPosts(ctx context.Context, posts []uuid.UUID) (map[uuid.UUID]string, error) {
	if len(posts) == 0 {
		return map[uuid.UUID]string{}, nil
	}
	return pgexec.Pairs[uuid.UUID, string](ctx, `SELECT post_uuid, room FROM call_sessions
		WHERE post_uuid = ANY($1::uuid[]) AND ended_at IS NULL`, pq.Array(posts))
}

// AdmittedThrough answers who came in through these links, to put them out of
// the call when the links end.
func AdmittedThrough(ctx context.Context, grants []uuid.UUID) ([]Admission, error) {
	if len(grants) == 0 {
		return nil, nil
	}
	return pgexec.Query(ctx, `SELECT room, user_uuid, identity, grant_id, session_id, admitted_at
		FROM call_admissions WHERE grant_id = ANY($1::uuid[])`, scanAdmission, pq.Array(grants))
}

// AdmittedIn answers everyone let into one call room through a link, to put
// them out when its links end together (a personal room closed).
func AdmittedIn(ctx context.Context, room string) ([]Admission, error) {
	return pgexec.Query(ctx, `SELECT room, user_uuid, identity, grant_id, session_id, admitted_at
		FROM call_admissions WHERE room = $1`, scanAdmission, room)
}

// AdmittedInLiveCalls answers everyone let in through a link who may be in a
// call going on now, to put them out when guest access is turned off.
func AdmittedInLiveCalls(ctx context.Context) ([]Admission, error) {
	return pgexec.Query(ctx, `SELECT a.room, a.user_uuid, a.identity, a.grant_id, a.session_id, a.admitted_at
		FROM call_admissions a JOIN call_sessions s ON s.room = a.room AND s.ended_at IS NULL
		WHERE a.session_id IS NULL OR a.session_id = s.id`, scanAdmission)
}

// ForgetAdmissionsTo forgets who came into a call by its invites, once the
// call is over: those admissions open nothing any more.
func ForgetAdmissionsTo(ctx context.Context, session uuid.UUID) error {
	_, err := pgexec.Exec(ctx, `DELETE FROM call_admissions WHERE session_id = $1`, session)
	return err
}

// ForgetAdmissionOf forgets that someone was let into the call in room
// through a link: a host put them out of it (business/LiveKit's
// RemoveFromCall), so they no longer count as maybe in it. A link lets them
// in again once the call that put them out has ended.
func ForgetAdmissionOf(ctx context.Context, room, identity string) error {
	_, err := pgexec.Exec(ctx, `DELETE FROM call_admissions WHERE room = $1 AND identity = $2`, room, identity)
	return err
}

func scanAdmission(r pgexec.Row) (Admission, error) {
	var a Admission
	var session uuid.NullUUID
	if err := r.Scan(&a.Room, &a.UserUUID, &a.Identity, &a.GrantID, &session, &a.At); err != nil {
		return a, err
	}
	if session.Valid {
		a.Session = &session.UUID
	}
	return a, nil
}
