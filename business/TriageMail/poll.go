package business

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"sync"
	"time"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	triageBusiness "github.com/akashc777/OneCamp/business/Triage"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	mailDomain "github.com/akashc777/OneCamp/domain/TriageMail"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/uploadsafe"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/pgexec"
	mailModel "github.com/akashc777/OneCamp/models/postgres/TriageMail"
	"github.com/google/uuid"
)

// The poller's bounds.
const (
	// PerPoll is the most messages one poll of a mailbox takes in: the most
	// triage arrivals (and their notices) a mailbox makes every Interval.
	PerPoll = 20
	// Interval is how often a mailbox is read; a pass runs every minute and
	// takes the mailboxes that are due.
	Interval = 2 * time.Minute
	// lease is how long a server holds a mailbox it polls: longer than the
	// slowest pass (batch mailboxes, together at once, pollTimeout and two
	// recordTimeouts each) and a minute more, so a mailbox is never claimed
	// twice while its pass still runs. batch is the mailboxes one pass claims;
	// together how many it polls at once.
	lease    = (batch+together-1)/together*(pollTimeout+2*recordTimeout) + time.Minute
	batch    = 10
	together = 4
	// pollTimeout bounds one mailbox's poll; recordTimeout the files taken
	// back from a task not made, and the hold and the finish after it, which
	// run even when the poll's budget ran out.
	pollTimeout   = 90 * time.Second
	recordTimeout = 10 * time.Second
	// unreachable is the failures in a row after which a mailbox pauses.
	unreachable = 24
)

// The reasons a mailbox pauses, which its admin reads. A paused mailbox is
// read again only when an admin saves it again.
const (
	PausedAdmin       = "admin"
	PausedAuth        = "auth_failed"
	PausedTLS         = "tls"
	PausedRefused     = "refused"
	PausedUnreachable = "unreachable"
	PausedTriageOff   = "triage_off"
	PausedOwnerLeft   = "owner_left"
	// PausedProjectDeleted: the project was deleted; its password was wiped.
	PausedProjectDeleted = mailDomain.PausedProjectDeleted
)

// The stores a poll uses: seams for its tests.
var (
	openBox        = func(ctx context.Context, b Box) (Session, error) { return openerFor()(ctx, b) }
	room           = triageBusiness.Room
	claimDue       = mailModel.ClaimDue
	claimKeys      = mailModel.ClaimKeys
	membersByEmail = userDomain.MembersByEmail
	ownerOf        = userDomain.ActorByUUID
	makeTask       = taskWriter.MakeTask
	storeFile      = func(ctx context.Context, f File, uploader uuid.UUID) (attachmentBusiness.Stored, error) {
		return attachmentBusiness.StoreUpload(ctx, bytes.NewReader(f.Data), int64(len(f.Data)), f.Name, uploader,
			attachmentBusiness.StoreOptions{Safe: uploadsafe.Options{}, FailClosed: true})
	}
	// removeFiles takes back the files stored for a task that wasn't made.
	removeFiles = attachmentBusiness.RemoveStoredAll
	holdAll     = triageBusiness.HoldAll
	finish      = mailModel.Finish
	setState    = mailModel.SetState
	decrypt     = helpers.DecryptSecret
	now         = time.Now
)

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "triage.mail", Every: time.Minute, ClaimsItsOwnWork: true, Run: PollDue})
}

// PollDue is the sweep's pass: it leases the mailboxes that are due (one
// statement, so each is polled by one server) and polls them, a few at a time.
func PollDue(ctx context.Context) error {
	boxes, err := claimDue(ctx, now().Add(lease), batch)
	if err != nil || len(boxes) == 0 {
		return err
	}
	sem := make(chan struct{}, together)
	var wg sync.WaitGroup
	for _, mb := range boxes {
		wg.Add(1)
		sem <- struct{}{}
		helpers.GoSafe(func() {
			defer func() { <-sem; wg.Done() }()
			pctx, cancel := context.WithTimeout(ctx, pollTimeout)
			defer cancel()
			if err := pollOne(pctx, mb); err != nil {
				helpers.LogErrorWithContext(ctx, "business/TriageMail poll %s: %s", mb.ID, errKind(err))
			}
		})
	}
	wg.Wait()
	return nil
}

// pollOne reads one leased mailbox: what triage has room for, the new mail
// by UID, each new message (its Message-ID key claimed first) as a task made
// as the admin who connected the mailbox, and all of them held in triage at
// once. Store calls: Room, one key claim, one member lookup, the owner, N
// tasks (each its attachments' puts), one HoldAll, one Finish transaction.
func pollOne(ctx context.Context, mb mailModel.Mailbox) error {
	space, on, err := room(ctx, mb.ProjectUUID)
	if err != nil {
		return failed(ctx, mb, err)
	}
	if !on {
		return pause(ctx, mb, PausedTriageOff)
	}
	if space == 0 {
		// Full: nothing is read until something is decided.
		return setState(ctx, mb.ID, mailModel.State{Failures: mb.Failures, NextPoll: now().Add(Interval), Polled: mb.UpdatedAt})
	}
	password, err := decrypt(mb.SecretSealed)
	if err != nil {
		return pause(ctx, mb, PausedAuth)
	}
	s, err := openBox(ctx, Box{Host: mb.Host, Port: mb.Port, TLS: mb.TLSMode, User: mb.Username, Password: password, Folder: mb.Folder, PrivateOK: mb.PrivateOK, ReadBudget: PollReadBudget})
	switch {
	case errors.Is(err, ErrAuth):
		return pause(ctx, mb, PausedAuth)
	case errors.Is(err, ErrTLS):
		return pause(ctx, mb, PausedTLS)
	case errors.Is(err, ErrBox):
		return pause(ctx, mb, PausedRefused)
	case err != nil:
		// A refused address is failed like one that can't be reached, as a
		// Test answers both: a name that points inside this server's network
		// leaves the same state as one that doesn't resolve.
		return failed(ctx, mb, err)
	}
	defer s.Close()
	validity := int64(s.Validity())
	done := mailModel.Done{MailboxID: mb.ID, ProjectUUID: mb.ProjectUUID, UIDValidity: validity, LastUID: mb.LastUID, NextPoll: now().Add(Interval), Polled: mb.UpdatedAt}
	if mb.UIDValidity == 0 {
		// First read: mail from now on (no backfill of the inbox).
		done.LastUID = int64(s.Next()) - 1
		return finish(ctx, done)
	}
	take := min(space, PerPoll)
	var got []Fetched
	var more bool
	if validity != mb.UIDValidity {
		// The folder was renumbered: what arrived since a day before the last
		// poll, with the Message-ID claim dropping what was taken already.
		since := now().Add(-24 * time.Hour)
		if mb.LastPolledAt != nil {
			since = mb.LastPolledAt.Add(-24 * time.Hour)
		}
		got, more, err = s.Since(ctx, since, take)
		// Everything since was read: the mark is the folder's top, never 0, or
		// the next poll would read it from UID 1. Hits left for another poll
		// (past take or past PollBytes): the mark is the last one read.
		done.LastUID = 0
		if !more {
			done.LastUID = int64(s.Next()) - 1
		}
	} else {
		got, more, err = s.After(ctx, uint32(mb.LastUID), take)
	}
	if err != nil {
		return failed(ctx, mb, err)
	}
	if len(got) == 0 {
		if more {
			// Hits, none read (gone between SEARCH and FETCH): nothing moves,
			// the next poll searches again.
			done.UIDValidity, done.LastUID = mb.UIDValidity, mb.LastUID
		}
		return finish(ctx, done)
	}
	// Each key once: one statement can't claim a key twice (a repeat inside
	// one poll is taken once).
	mails := make([]Mail, len(got))
	var keys [][]byte
	seen := make(map[string]bool, len(got))
	for i, f := range got {
		mails[i] = Parse(f)
		if k := string(mails[i].Key); !seen[k] {
			seen[k] = true
			keys = append(keys, mails[i].Key)
		}
		done.LastUID = max(done.LastUID, int64(f.UID))
	}
	fresh, made, err := claimKeys(ctx, mb.ProjectUUID, mb.ID, keys)
	if err != nil {
		return failed(ctx, mb, err)
	}
	// The mark passes a message only once a task was made from it, now or by an
	// earlier poll: one another poll holds, or whose task this poll can't make,
	// is read again (at least once; the key claim keeps it to one task).
	stop := func(uid uint32) { done.LastUID = min(done.LastUID, int64(uid)-1) }
	isNew, isMade := keySet(fresh), keySet(made)
	taken := make(map[string]bool, len(fresh))
	var todo []Mail
	var todoUID []uint32
	var senders []string
	for i, m := range mails {
		switch k := string(m.Key); {
		case taken[k], isMade[k]:
			// A repeat inside this poll is taken once; one an earlier poll made is done.
		case isNew[k]:
			taken[k] = true
			todo, todoUID = append(todo, m), append(todoUID, got[i].UID)
			if m.From != "" {
				senders = append(senders, m.From)
			}
		default:
			stop(got[i].UID)
		}
	}
	if len(todo) == 0 {
		return finish(ctx, done)
	}
	members, err := membersByEmail(ctx, senders)
	if err != nil {
		return failed(ctx, mb, err)
	}
	// Only a connecting admin who is gone pauses the mailbox; any other error
	// reading them is a hiccup, backed off and tried again.
	owner, err := ownerOf(helpers.WithSystemRead(ctx), mb.ConnectedBy)
	switch {
	case errors.Is(err, userDomain.ErrNoActor), err == nil && owner == nil:
		return pause(ctx, mb, PausedOwnerLeft)
	case err != nil:
		return failed(ctx, mb, err)
	}
	// What a stranger wrote is filed as written outside, as a public form's is
	// (helpers.WithPublicSubmission): an agent or rule it wakes never runs with
	// the connecting admin's reach on it.
	ctx = helpers.WithSubmissionDoor(ctx, string(triageBusiness.FromMail))
	var arrivals []triageBusiness.Arrival
	// unmade is the files stored for the task that couldn't be made: they go
	// back after the loop (the poll that makes it stores them again).
	var unmade []*dgraphStruct.DgraphAttachment
	lost := false
	for i, m := range todo {
		atts := files(ctx, m, mb.ConnectedBy)
		task, err := makeTask(ctx, owner, mb.ProjectUUID, taskWriter.NewTask{
			Name: subjectOf(m), Description: describe(m), Status: dgraphStruct.TASK_STATUS_TODO, Attachments: atts,
		})
		if err != nil {
			unmade = atts
			// The connecting admin can't make work here any more: stop, and
			// leave the rest for an admin who can. Anything else (a store
			// hiccup) is tried again: this and the rest are given back and the
			// mark stops below them.
			if lost = len(arrivals) == 0 && ownerLost(err); lost {
				break
			}
			helpers.LogErrorWithContext(ctx, "business/TriageMail make task err: %+v", err)
			done.Release = keysOf(todo[i:])
			stop(todoUID[i])
			break
		}
		from := triageBusiness.Requester{Name: m.FromName, Email: m.From}
		if id, ok := members[m.From]; ok {
			from.UserUUID = &id
		}
		arrivals = append(arrivals, triageBusiness.Arrival{TaskID: task, From: from})
		done.Keys, done.Tasks = append(done.Keys, m.Key), append(done.Tasks, task)
	}
	dropFiles(ctx, unmade)
	if lost {
		return pause(ctx, mb, PausedOwnerLeft)
	}
	// What was made is recorded even when the poll's budget ran out mid-loop
	// (a task made, then the next failed on the ended ctx): the hold and the
	// finish run on their own deadline, or a later poll makes it again.
	rec, cancel := context.WithTimeout(pgexec.Detached(ctx), recordTimeout)
	defer cancel()
	if _, err := holdAll(rec, mb.ProjectUUID, triageBusiness.FromMail, arrivals); err != nil {
		helpers.LogErrorWithContext(rec, "business/TriageMail hold err: %+v", err)
	}
	return finish(rec, done)
}

// keySet is keys as a set.
func keySet(keys [][]byte) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[string(k)] = true
	}
	return out
}

// keysOf is the keys of mails.
func keysOf(mails []Mail) [][]byte {
	out := make([][]byte, len(mails))
	for i, m := range mails {
		out[i] = m.Key
	}
	return out
}

// ownerLost reports whether a task couldn't be made because the connecting
// admin can't make work in the project any more.
func ownerLost(err error) bool {
	var refused *workAccess.Refusal
	return errors.Is(err, taskBusiness.ErrTaskNotVisible) || errors.As(err, &refused)
}

// subjectOf is a message's task name.
func subjectOf(m Mail) string {
	if m.Subject != "" {
		return m.Subject
	}
	return "Mail from " + helpers.FirstNonEmpty(m.FromName, m.From, "an unknown sender")
}

// describe is a message's task description: who sent it, then its text,
// escaped (helpers.PlainTextToHTML: never markup, never a mention), and what
// was left out.
func describe(m Mail) string {
	who := html.EscapeString(helpers.FirstNonEmpty(m.FromName, m.From, "an unknown sender"))
	if m.FromName != "" && m.From != "" {
		who += " &lt;" + html.EscapeString(m.From) + "&gt;"
	}
	d := "<p>Arrived by mail from <strong>" + who + "</strong>.</p>" + helpers.PlainTextToHTML(m.Text)
	var notes []string
	if m.TooLarge {
		notes = append(notes, fmt.Sprintf("The message is over %d MB, so only its subject and sender were taken.", MaxMessage>>20))
	}
	if m.Cut {
		notes = append(notes, "The message was cut short.")
	}
	if m.Dropped > 0 {
		notes = append(notes, fmt.Sprintf("%d attachment(s) over the limits (%d a message, %d MB each) were left out.", m.Dropped, MaxFiles, MaxFile>>20))
	}
	for _, n := range notes {
		d += "<p><em>" + html.EscapeString(n) + "</em></p>"
	}
	return d
}

// files keeps a message's attachments in the file store (scanned, a
// stranger's file kept only when clean) as the task's attachments; one that
// isn't kept is left out.
func files(ctx context.Context, m Mail, uploader uuid.UUID) []*dgraphStruct.DgraphAttachment {
	var out []*dgraphStruct.DgraphAttachment
	for _, f := range m.Files {
		s, err := storeFile(ctx, f, uploader)
		if err != nil {
			continue
		}
		out = append(out, &dgraphStruct.DgraphAttachment{Uuid: s.ID.String(), FileName: s.SafeName, ObjectKey: s.ObjKey,
			Type: attachmentBusiness.KindOf(s.SafeName), RawType: s.ContentType, Size: int(s.Size)})
	}
	return out
}

// dropFiles takes back, in one request, the files stored for a task that
// wasn't made (no row names them), on its own deadline: a poll whose budget
// ran out is a likely reason the task wasn't made.
func dropFiles(ctx context.Context, atts []*dgraphStruct.DgraphAttachment) {
	if len(atts) == 0 {
		return
	}
	keys := make([]string, len(atts))
	for i, a := range atts {
		keys[i] = a.ObjectKey
	}
	rctx, cancel := context.WithTimeout(pgexec.Detached(ctx), recordTimeout)
	defer cancel()
	if err := removeFiles(rctx, keys); err != nil {
		helpers.LogErrorWithContext(rctx, "business/TriageMail drop files err: %+v", err)
	}
}

// pause stops polling a mailbox until an admin saves it again.
func pause(ctx context.Context, mb mailModel.Mailbox, reason string) error {
	return record(ctx, mb.ID, mailModel.State{Failures: mb.Failures, PausedReason: reason, NextPoll: now(), Polled: mb.UpdatedAt})
}

// record writes a pause or a failure on its own deadline, as Finish records
// what a poll made: a poll the pollTimeout ended still backs off and, after
// its count, pauses, so a server that never finishes can't keep its mailbox
// first in ClaimDue and a poll slot busy.
func record(ctx context.Context, id uuid.UUID, st mailModel.State) error {
	rec, cancel := context.WithTimeout(pgexec.Detached(ctx), recordTimeout)
	defer cancel()
	return setState(rec, id, st)
}

// failed backs a mailbox off (4, 8, 16 ... 60 minutes) after a network or
// store error, and pauses it after unreachable failures in a row.
func failed(ctx context.Context, mb mailModel.Mailbox, cause error) error {
	n := mb.Failures + 1
	if n >= unreachable {
		mb.Failures = n
		_ = pause(ctx, mb, PausedUnreachable)
		return cause
	}
	wait := min(Interval<<min(n, 5), time.Hour)
	if err := record(ctx, mb.ID, mailModel.State{Failures: n, NextPoll: now().Add(wait), Polled: mb.UpdatedAt}); err != nil {
		return err
	}
	return cause
}

// errKind is an error as a log may name it: never the server's own words,
// which can echo the account.
func errKind(err error) string {
	switch {
	case errors.Is(err, ErrAuth):
		return "login refused"
	case errors.Is(err, ErrTLS):
		return "tls failed"
	case errors.Is(err, ErrRefused):
		return "address refused"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	}
	return "failed"
}
