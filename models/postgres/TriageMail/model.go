// Package models (TriageMail) runs the mail door's query text
// (domain/TriageMail) through models/pgexec.
package models

import (
	"context"
	"time"

	mailDomain "github.com/akashc777/OneCamp/domain/TriageMail"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Mailbox is a project's connected mailbox. SecretSealed never leaves the
// server: business/TriageMail opens it only to dial.
type Mailbox struct {
	ID           uuid.UUID
	ProjectUUID  uuid.UUID
	Host         string
	Port         int
	TLSMode      string
	Username     string
	SecretSealed string `json:"-"`
	Folder       string
	PrivateOK    bool
	UIDValidity  int64
	LastUID      int64
	Failures     int
	PausedReason string
	LastPolledAt *time.Time
	ConnectedBy  uuid.UUID
	// UpdatedAt is the row as read: a poll records itself only on the row
	// it claimed (Done.Polled, State.Polled).
	UpdatedAt time.Time
}

func scanMailbox(r pgexec.Row) (m Mailbox, err error) {
	err = r.Scan(&m.ID, &m.ProjectUUID, &m.Host, &m.Port, &m.TLSMode, &m.Username, &m.SecretSealed, &m.Folder, &m.PrivateOK,
		&m.UIDValidity, &m.LastUID, &m.Failures, &m.PausedReason, &m.LastPolledAt, &m.ConnectedBy, &m.UpdatedAt)
	return
}

// ByProject is a project's live mailbox, nil for none.
func ByProject(ctx context.Context, projectUUID uuid.UUID) (*Mailbox, error) {
	m, ok, err := pgexec.QueryOne(ctx, mailDomain.MailboxOf, scanMailbox, projectUUID)
	if err != nil || !ok {
		return nil, err
	}
	return &m, nil
}

// Save connects or changes a project's mailbox; an empty SecretSealed keeps
// the one kept, and an empty PausedReason is none.
func Save(ctx context.Context, m Mailbox) error {
	_, err := pgexec.Exec(ctx, mailDomain.SaveMailbox, m.ID, m.ProjectUUID, m.Host, m.Port, m.TLSMode, m.Username, m.SecretSealed,
		m.Folder, m.PrivateOK, nullIfEmpty(m.PausedReason), m.ConnectedBy)
	return err
}

// Remove disconnects a project's mailbox: soft-deleted, its secret wiped.
// removed false is no mailbox to remove.
func Remove(ctx context.Context, projectUUID, by uuid.UUID) (removed bool, err error) {
	n, err := pgexec.Exec(ctx, mailDomain.RemoveMailbox, projectUUID, by)
	return n > 0, err
}

// ClaimDue leases at most limit due mailboxes until until.
func ClaimDue(ctx context.Context, until time.Time, limit int) ([]Mailbox, error) {
	return pgexec.Query(ctx, mailDomain.ClaimDue, scanMailbox, until, limit)
}

// ClaimKeys claims a poll's keys and answers the ones this claim took
// (fresh) and the ones a task was made from already (made); any other key is
// another poll's live claim.
func ClaimKeys(ctx context.Context, projectUUID, mailboxUUID uuid.UUID, keys [][]byte) (fresh, made [][]byte, err error) {
	if len(keys) == 0 {
		return nil, nil, nil
	}
	type state struct {
		key         []byte
		fresh, made bool
	}
	rows, err := pgexec.Query(ctx, mailDomain.ClaimKeys, func(r pgexec.Row) (s state, err error) { err = r.Scan(&s.key, &s.fresh, &s.made); return },
		projectUUID, mailboxUUID, pq.ByteaArray(keys))
	for _, s := range rows {
		if s.fresh {
			fresh = append(fresh, s.key)
		} else if s.made {
			made = append(made, s.key)
		}
	}
	return fresh, made, err
}

// Done is a poll that read its mailbox: the keys it finished and the task
// each made (in step), and where the mailbox stands.
type Done struct {
	MailboxID   uuid.UUID
	ProjectUUID uuid.UUID
	Keys        [][]byte
	Tasks       []uuid.UUID
	// Release is the keys this poll claimed and made no task from.
	Release     [][]byte
	UIDValidity int64
	LastUID     int64
	NextPoll    time.Time
	// Polled is the mailbox's UpdatedAt as the poll claimed it.
	Polled time.Time
}

// Finish records a poll in one transaction: the keys' tasks, the claims it
// gives back, and the mailbox's UIDs, so a restart neither reads a message twice nor skips one.
func Finish(ctx context.Context, d Done) error {
	return pgexec.InTx(ctx, func(ctx context.Context) error {
		if len(d.Keys) > 0 {
			tasks := make([]string, len(d.Tasks))
			for i, t := range d.Tasks {
				tasks[i] = t.String()
			}
			if _, err := pgexec.Exec(ctx, mailDomain.FinishKeys, d.ProjectUUID, pq.ByteaArray(d.Keys), pq.Array(tasks)); err != nil {
				return err
			}
		}
		if len(d.Release) > 0 {
			if _, err := pgexec.Exec(ctx, mailDomain.ReleaseKeys, d.ProjectUUID, d.MailboxID, pq.ByteaArray(d.Release)); err != nil {
				return err
			}
		}
		_, err := pgexec.Exec(ctx, mailDomain.FinishPoll, d.MailboxID, d.UIDValidity, d.LastUID, d.NextPoll, d.Polled)
		return err
	})
}

// State is a poll that didn't read its mailbox: its failures so far, why it
// paused ("" for not), and when it is due again.
type State struct {
	Failures     int
	PausedReason string
	NextPoll     time.Time
	// Polled is the mailbox's UpdatedAt as the poll claimed it.
	Polled time.Time
}

// SetState records a poll that didn't read its mailbox.
func SetState(ctx context.Context, id uuid.UUID, s State) error {
	_, err := pgexec.Exec(ctx, mailDomain.SetState, id, s.Failures, nullIfEmpty(s.PausedReason), s.NextPoll, s.Polled)
	return err
}

// StopForProject stops a deleted project's mailbox (mailDomain.StopForProject),
// in the transaction ctx carries.
func StopForProject(ctx context.Context, projectUUID uuid.UUID) error {
	_, err := pgexec.Exec(ctx, mailDomain.StopForProject, projectUUID)
	return err
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
