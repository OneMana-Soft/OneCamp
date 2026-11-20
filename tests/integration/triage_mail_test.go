//go:build integration
// +build integration

package integration_test

// The mail door end to end (linear-21), against real Postgres 12 and Dgraph:
// the demo's stub mailbox (never real mail) is connected to a project with
// triage on; its messages arrive as tasks held in triage with source "mail",
// the repeated Message-ID once; a member can't connect a mailbox; a saved
// password is sealed and never read back; removing the mailbox wipes it and
// polling stops.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestTriageMail' -v

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	triageBusiness "github.com/akashc777/OneCamp/business/Triage"
	triageMail "github.com/akashc777/OneCamp/business/TriageMail"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/pgexec"
	mailModel "github.com/akashc777/OneCamp/models/postgres/TriageMail"
	"github.com/google/uuid"
)

func TestTriageMailStubDeliversOnceIntoTriage(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	ana := w.account("ana")
	project := uuid.MustParse(w.project)
	if _, err := policyBusiness.Admin.SetIn(ctx, policyBusiness.ProjectsTriage.Key, policyBusiness.Scope{Kind: policyBusiness.ScopeProject, ID: w.project},
		json.RawMessage("true"), policyBusiness.Changer{UserID: ana.UserPostgresInfo.Id, Email: "ana@acme.test"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEMO_MODE", "true")
	if err := triageMail.ConnectDemo(ctx, project, ana.UserPostgresInfo.Id); err != nil {
		t.Fatalf("connecting the stub: %v", err)
	}
	mail := func() []triageBusiness.Request {
		t.Helper()
		q, err := triageBusiness.Triage.Waiting(ctx, &ana, project)
		if err != nil {
			t.Fatal(err)
		}
		var out []triageBusiness.Request
		for _, r := range q {
			if r.Source == "mail" {
				out = append(out, r)
			}
		}
		return out
	}
	// The first poll marks where the mailbox stands; the next one reads it.
	for i := 0; i < 2; i++ {
		w.exec(`UPDATE triage_mailboxes SET next_poll_at = NOW() - INTERVAL '1 second' WHERE project_uuid = $1`, project)
		if err := triageMail.PollDue(ctx); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}
	got := mail()
	if len(got) != 2 {
		state, _ := pgexec.Scalar[string](ctx, `SELECT concat_ws(' ', uid_validity, last_uid, failures, paused_reason) FROM triage_mailboxes WHERE project_uuid = $1`, project)
		t.Logf("the mailbox: validity last failures paused = %s", state)
		t.Fatalf("%d arrived by mail, want 2 (the repeat dropped): %+v", len(got), got)
	}
	emails := got[0].RequesterEmail + " " + got[1].RequesterEmail
	if !strings.Contains(emails, "priya@acme.example") || !strings.Contains(emails, "maya.chen@cast.demo.onemana.dev") {
		t.Errorf("the senders: %q", emails)
	}
	if n, err := pgexec.Scalar[int](ctx, `SELECT count(*) FROM triage_mail_seen WHERE project_uuid = $1 AND task_uuid IS NOT NULL`, project); err != nil || n != 2 {
		t.Errorf("finished keys %d, %v", n, err)
	}
	w.exec(`UPDATE triage_mailboxes SET next_poll_at = NOW() - INTERVAL '1 second' WHERE project_uuid = $1`, project)
	if err := triageMail.PollDue(ctx); err != nil || len(mail()) != 2 {
		t.Errorf("a third poll took more: %v", err)
	}
}

func TestTriageMailAdminsOnlyAndTheSecretStaysSealed(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	ana, ben := w.account("ana"), w.account("ben")
	project := uuid.MustParse(w.project)
	t.Setenv("DEMO_MODE", "false")
	in := triageMail.Settings{Host: "imap.example.com", Port: 993, TLS: "tls", Username: "help@example.com", Password: "hunter2-mail-secret"}
	member := workAccess.Standing{ProjectUUID: w.project, Role: workAccess.RoleMember}
	if _, err := triageMail.Save(ctx, &ben, member, project, in); err != triageMail.ErrNotAdmin {
		t.Fatalf("a member saved a mailbox: %v", err)
	}
	admin := workAccess.Standing{ProjectUUID: w.project, Role: workAccess.RoleAdmin}
	v, err := triageMail.Save(ctx, &ana, admin, project, in)
	if err != nil || v.ConnectedAs != "help@example.com" {
		t.Fatalf("saving: %+v %v", v, err)
	}
	sealed, err := pgexec.Scalar[string](ctx, `SELECT secret_sealed FROM triage_mailboxes WHERE project_uuid = $1 AND deleted_at IS NULL`, project)
	if err != nil || sealed == "" || strings.Contains(sealed, "hunter2") {
		t.Fatalf("the secret kept: %q %v", sealed, err)
	}
	read, err := triageMail.Read(ctx, admin, project)
	raw, _ := json.Marshal(read)
	if err != nil || strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), sealed) {
		t.Fatalf("the read: %s %v", raw, err)
	}
	// An empty secret keeps the kept one only on its own server and user, in
	// SQL as in business (keepsPassword): another host, user, port or TLS
	// mode clears it. Each row starts from the sealed secret kept.
	same := mailModel.Mailbox{ID: uuid.New(), ProjectUUID: project, Host: "imap.example.com", Port: 993, TLSMode: "tls", Username: "help@example.com",
		Folder: "Support", ConnectedBy: ana.UserPostgresInfo.Id}
	reseal := same
	reseal.SecretSealed = sealed
	moved, otherUser, otherPort, otherTLS := same, same, same, same
	moved.Host = "imap.example.org"
	otherUser.Username = "sales@example.com"
	otherPort.Port = 143
	otherTLS.TLSMode = "starttls"
	for _, c := range []struct {
		name string
		m    mailModel.Mailbox
		want string
	}{{"same", same, sealed}, {"moved host", moved, ""}, {"moved user", otherUser, ""}, {"port", otherPort, ""}, {"tls", otherTLS, ""}} {
		if err := mailModel.Save(ctx, reseal); err != nil {
			t.Fatal(err)
		}
		if err := mailModel.Save(ctx, c.m); err != nil {
			t.Fatal(err)
		}
		if kept, err := pgexec.Scalar[string](ctx, `SELECT secret_sealed FROM triage_mailboxes WHERE project_uuid = $1 AND deleted_at IS NULL`, project); err != nil || kept != c.want {
			t.Fatalf("an empty secret saved after a change of %s left %q (%v), want %q", c.name, kept, err, c.want)
		}
	}
	if err := triageMail.Remove(ctx, &ana, admin, project); err != nil {
		t.Fatal(err)
	}
	if left, err := pgexec.Scalar[string](ctx, `SELECT secret_sealed FROM triage_mailboxes WHERE project_uuid = $1`, project); err != nil || left != "" {
		t.Fatalf("after removing, the secret is %q (%v)", left, err)
	}
	if due, err := pgexec.Scalar[int](ctx, `SELECT count(*) FROM triage_mailboxes WHERE project_uuid = $1 AND deleted_at IS NULL`, project); err != nil || due != 0 {
		t.Fatalf("a removed mailbox is still live: %d %v", due, err)
	}
	if err := triageMail.PollDue(ctx); err != nil {
		t.Fatalf("a poll after removing: %v", err)
	}
	if r, err := triageMail.Read(ctx, admin, project); err != nil || r.Connected {
		t.Errorf("after removing it reads %+v, %v", r, err)
	}
}

// A save while a poll is in flight wins over that poll's own record (CR-410
// N1, "mail from now on"): the poll read the old box to 7|400, the admin
// re-points it (validity back to 0), and the poll's finish on the old box
// must not write 7|450 over the new one. A pause saved mid-poll survives the
// poll's state write the same way.
func TestTriageMailASaveMidPollWinsOverThePoll(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	project := uuid.MustParse(w.project)
	by := w.account("ana").UserPostgresInfo.Id
	box := func(host, paused string) mailModel.Mailbox {
		return mailModel.Mailbox{ID: uuid.New(), ProjectUUID: project, Host: host, Port: 993, TLSMode: "tls", Username: "help@example.com",
			SecretSealed: "sealed-example", Folder: "INBOX", PausedReason: paused, ConnectedBy: by}
	}
	read := func() *mailModel.Mailbox {
		t.Helper()
		m, err := mailModel.ByProject(ctx, project)
		if err != nil || m == nil {
			t.Fatalf("reading the mailbox: %v %v", m, err)
		}
		return m
	}
	if err := mailModel.Save(ctx, box("imap.old.example", "")); err != nil {
		t.Fatal(err)
	}
	first := read()
	if err := mailModel.Finish(ctx, mailModel.Done{MailboxID: first.ID, ProjectUUID: project, UIDValidity: 7, LastUID: 400, NextPoll: time.Now(), Polled: first.UpdatedAt}); err != nil {
		t.Fatal(err)
	}
	polled := read() // the row as the next poll claims it
	if err := mailModel.Save(ctx, box("imap.new.example", "")); err != nil {
		t.Fatal(err)
	}
	if err := mailModel.Finish(ctx, mailModel.Done{MailboxID: polled.ID, ProjectUUID: project, UIDValidity: 7, LastUID: 450, NextPoll: time.Now(), Polled: polled.UpdatedAt}); err != nil {
		t.Fatal(err)
	}
	if m := read(); m.Host != "imap.new.example" || m.UIDValidity != 0 {
		t.Fatalf("after a save mid-poll and the poll's finish: %s|%d|%d, want imap.new.example|0 (the new box starts from what arrives next)", m.Host, m.UIDValidity, m.LastUID)
	}
	polled = read()
	if err := mailModel.Save(ctx, box("imap.new.example", triageMail.PausedAdmin)); err != nil {
		t.Fatal(err)
	}
	if err := mailModel.SetState(ctx, polled.ID, mailModel.State{NextPoll: time.Now(), Polled: polled.UpdatedAt}); err != nil {
		t.Fatal(err)
	}
	if m := read(); m.PausedReason != triageMail.PausedAdmin {
		t.Fatalf("a pause saved mid-poll reads %q after the poll's state: want %q kept", m.PausedReason, triageMail.PausedAdmin)
	}
}

// Deleting a project (soft) stops its mailbox in the delete's own
// transaction: never polled again and its sealed password wiped (CR-409 N1).
// Restoring the project leaves the mailbox paused, asking for the password.
func TestTriageMailADeletedProjectStopsItsMailbox(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	ana := w.account("ana")
	project := uuid.MustParse(w.project)
	t.Setenv("DEMO_MODE", "false")
	admin := workAccess.Standing{ProjectUUID: w.project, Role: workAccess.RoleAdmin}
	in := triageMail.Settings{Host: "imap.example.com", Port: 993, TLS: "tls", Username: "help@example.com", Password: "pw-example-only"}
	if _, err := triageMail.Save(ctx, &ana, admin, project, in); err != nil {
		t.Fatal(err)
	}
	info := &dgraphStruct.DgraphProject{Uuid: w.project, Name: "mail door"}
	if err := projectBusiness.ArchiveProjectByProjectUUID(ctx, project, info); err != nil {
		t.Fatal(err)
	}
	state := func(when string) {
		t.Helper()
		m, err := mailModel.ByProject(ctx, project)
		if err != nil || m == nil || m.SecretSealed != "" || m.PausedReason != triageMail.PausedProjectDeleted {
			if m == nil {
				t.Fatalf("%s: no mailbox (%v)", when, err)
			}
			t.Fatalf("%s: paused %q, secret wiped %v (%v): want it paused as %s with its secret wiped", when, m.PausedReason, m.SecretSealed == "", err, triageMail.PausedProjectDeleted)
		}
	}
	state("after the delete")
	if err := projectBusiness.UnArchiveProjectUUID(ctx, project, info); err != nil {
		t.Fatal(err)
	}
	state("after the restore")
	if _, err := triageMail.Save(ctx, &ana, admin, project, triageMail.Settings{Host: in.Host, Port: in.Port, TLS: in.TLS, Username: in.Username}); err != triageMail.ErrNoPassword {
		t.Fatalf("saving the restored mailbox with no password: %v, want ErrNoPassword", err)
	}
}
