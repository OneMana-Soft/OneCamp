package models

import (
	"context"
	"time"

	bookingDomain "github.com/akashc777/OneCamp/domain/Booking"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// A team page's host is invited, then accepted, until they leave or the
// owner takes them off (left): every delete is soft.
const (
	HostInvited  = "invited"
	HostAccepted = "accepted"
	HostLeft     = "left"
)

// Host is one member's row on a team page.
type Host struct {
	PageId     uuid.UUID  `json:"-"`
	UserId     uuid.UUID  `json:"user_id"`
	Name       string     `json:"name"`
	State      string     `json:"state"`
	InvitedAt  time.Time  `json:"invited_at"`
	AcceptedAt *time.Time `json:"-"`
	LeftAt     *time.Time `json:"-"`
}

// hostState is a row's state from its times.
func hostState(accepted, left *time.Time) string {
	switch {
	case left != nil:
		return HostLeft
	case accepted != nil:
		return HostAccepted
	}
	return HostInvited
}

func scanUUID(r pgexec.Row) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.Scan(&id)
	return id, err
}

// EligibleHosts is which of ids can host a team page: active human members.
func EligibleHosts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	ok, err := pgexec.Query(ctx, bookingDomain.EligibleHosts, scanUUID, pq.Array(ids))
	out := make(map[uuid.UUID]bool, len(ok))
	for _, id := range ok {
		out[id] = true
	}
	return out, err
}

// SetHosts makes hosts page's host list, in one transaction: who is new (or
// back after leaving) is invited, who is off it leaves. It returns who was
// invited now, to be told. It runs to its end when the request goes: the
// page is already saved (HoldPage's way), and InTx's TxTimeout bounds it.
func SetHosts(ctx context.Context, page uuid.UUID, hosts []uuid.UUID) ([]uuid.UUID, error) {
	if hosts == nil {
		hosts = []uuid.UUID{} // NULL would keep nobody
	}
	var invited []uuid.UUID
	err := pgexec.InTx(context.WithoutCancel(ctx), func(ctx context.Context) error {
		if _, err := pgexec.Exec(ctx, bookingDomain.DropHosts, page, pq.Array(hosts)); err != nil || len(hosts) == 0 {
			return err
		}
		var err error
		invited, err = pgexec.Query(ctx, bookingDomain.InviteHosts, scanUUID, page, pq.Array(hosts))
		return err
	})
	return invited, err
}

// HostsOfPages is every host row of pages, each with its state; Name is the
// caller's to fill.
func HostsOfPages(ctx context.Context, pages []uuid.UUID) ([]Host, error) {
	return pgexec.Query(ctx, bookingDomain.HostsOfPages, func(r pgexec.Row) (Host, error) {
		var h Host
		err := r.Scan(&h.PageId, &h.UserId, &h.InvitedAt, &h.AcceptedAt, &h.LeftAt)
		h.State = hostState(h.AcceptedAt, h.LeftAt)
		return h, err
	}, pq.Array(pages))
}

// Hosted is a page a member hosts or is invited to, as their list shows it.
type Hosted struct {
	PageId    uuid.UUID `json:"page_id"`
	OwnerId   uuid.UUID `json:"owner_id"`
	OwnerName string    `json:"owner_name"`
	Title     string    `json:"title"`
	Slug      string    `json:"slug"`
	State     string    `json:"state"`
	InvitedAt time.Time `json:"invited_at"`
}

// Hosting is the pages member hosts or is invited to, through their own rows.
func Hosting(ctx context.Context, member uuid.UUID) ([]Hosted, error) {
	list, err := pgexec.Query(ctx, bookingDomain.Hosting, func(r pgexec.Row) (Hosted, error) {
		var h Hosted
		var accepted, left *time.Time
		err := r.Scan(&h.PageId, &h.OwnerId, &h.Title, &h.Slug, &h.InvitedAt, &accepted, &left)
		h.State = hostState(accepted, left)
		return h, err
	}, member)
	if list == nil {
		list = []Hosted{}
	}
	return list, err
}

// AnswerHost is member accepting (or leaving) page through their own row,
// and the page's owner; false when they have no such row on a live page.
func AnswerHost(ctx context.Context, page, member uuid.UUID, accept bool) (uuid.UUID, bool, error) {
	q := bookingDomain.LeaveHost
	if accept {
		q = bookingDomain.AcceptHost
	}
	return pgexec.QueryOne(ctx, q, scanUUID, page, member)
}
