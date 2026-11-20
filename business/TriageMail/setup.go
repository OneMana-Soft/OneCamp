package business

import (
	"context"
	"errors"
	"strings"
	"time"

	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	"github.com/akashc777/OneCamp/helpers"
	mailModel "github.com/akashc777/OneCamp/models/postgres/TriageMail"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Who may do what with a project's mailbox: its admins (NeedManage), in the
// app. They connect it, change it, pause or resume it, test it and remove
// it, as they switch the project's triage on and set its rota (H1). A token
// can't: tokens act as members, and members can't. Members never read it:
// what arrives shows in triage like every door's work.
var (
	ErrNotAdmin     = errors.New("triage mail: only the project's admins connect its mailbox")
	ErrThroughToken = errors.New("triage mail: a mailbox is connected in the app, not through a token")
	ErrNoMailbox    = errors.New("triage mail: the project has no mailbox")
	ErrNoPassword   = errors.New("triage mail: a new mailbox needs its password")
)

// Settings is what an admin saves: where the mailbox is, its account, and
// whether it is paused. An empty Password keeps the one kept.
type Settings struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	TLS      string `json:"tls"`
	Username string `json:"username"`
	Password string `json:"password"`
	Folder   string `json:"folder"`
	Paused   bool   `json:"paused"`
}

// View is a mailbox as its admins read it: "connected as" its account,
// never its password, sealed or not.
type View struct {
	Connected    bool       `json:"connected"`
	ConnectedAs  string     `json:"connected_as,omitempty"`
	Host         string     `json:"host,omitempty"`
	Port         int        `json:"port,omitempty"`
	TLS          string     `json:"tls,omitempty"`
	Folder       string     `json:"folder,omitempty"`
	PausedReason string     `json:"paused_reason,omitempty"`
	LastPolledAt *time.Time `json:"last_polled_at,omitempty"`
}

// The store the routes use: seams for their tests.
var (
	byProject = mailModel.ByProject
	saveBox   = mailModel.Save
	removeBox = mailModel.Remove
	seal      = helpers.EncryptSecret
	testBox   = func(ctx context.Context, b Box) error {
		s, err := openerFor()(ctx, b)
		if err != nil {
			return err
		}
		s.Close()
		return nil
	}
)

// manages is the one check every mailbox route makes: in the app, as one of
// the project's admins. s is the standing the route checked.
func manages(ctx context.Context, s workAccess.Standing) error {
	if helpers.ActsThroughToken(ctx) {
		return ErrThroughToken
	}
	if !s.Can(workAccess.NeedManage) {
		return ErrNotAdmin
	}
	return nil
}

func viewOf(m *mailModel.Mailbox) View {
	if m == nil {
		return View{}
	}
	return View{Connected: true, ConnectedAs: m.Username, Host: m.Host, Port: m.Port, TLS: m.TLSMode, Folder: m.Folder,
		PausedReason: m.PausedReason, LastPolledAt: m.LastPolledAt}
}

// Read is a project's mailbox for its admins.
func Read(ctx context.Context, s workAccess.Standing, projectID uuid.UUID) (View, error) {
	if err := manages(ctx, s); err != nil {
		return View{}, err
	}
	m, err := byProject(ctx, projectID)
	if err != nil {
		return View{}, err
	}
	return viewOf(m), nil
}

// boxOf is what an admin saved as a mailbox to dial: on the demo, the stub's
// placeholder, with no password kept.
func boxOf(who *userModels.UserInfo, in Settings) Box {
	b := Box{Host: strings.ToLower(strings.TrimSpace(in.Host)), Port: in.Port, TLS: in.TLS, User: strings.TrimSpace(in.Username),
		Password: in.Password, Folder: strings.TrimSpace(in.Folder), PrivateOK: who != nil && who.UserPostgresInfo.IsAdmin}
	if b.Folder == "" {
		b.Folder = "INBOX"
	}
	if helpers.DemoMode() {
		b.Host, b.Port, b.TLS, b.Password = "demo.invalid", 993, "tls", ""
	}
	return b
}

// Save connects a project's mailbox or changes it, as who, one of its
// admins. The password is sealed (helpers/secretbox.go) before it is kept
// and never returned; a new mailbox needs one. Saving resumes a paused
// mailbox unless in.Paused; a change of host, account or folder reads from
// what arrives next. Which network may be dialled is who's: their own
// network only for a workspace admin.
func Save(ctx context.Context, who *userModels.UserInfo, s workAccess.Standing, projectID uuid.UUID, in Settings) (View, error) {
	if err := manages(ctx, s); err != nil {
		return View{}, err
	}
	b := boxOf(who, in)
	if err := checkBox(b); err != nil {
		return View{}, err
	}
	if b.User == "" || len(b.User) > 320 || len(b.Folder) > 200 || len(in.Password) > 1024 {
		return View{}, ErrBox
	}
	sealed := ""
	if b.Password != "" {
		var err error
		if sealed, err = seal(b.Password); err != nil {
			return View{}, err
		}
	} else if !helpers.DemoMode() {
		have, err := byProject(ctx, projectID)
		if err != nil {
			return View{}, err
		}
		if !keepsPassword(have, b) {
			return View{}, ErrNoPassword
		}
	}
	m := mailModel.Mailbox{ID: uuid.New(), ProjectUUID: projectID, Host: b.Host, Port: b.Port, TLSMode: b.TLS, Username: b.User,
		SecretSealed: sealed, Folder: b.Folder, PrivateOK: b.PrivateOK, ConnectedBy: who.UserPostgresInfo.Id}
	if in.Paused {
		m.PausedReason = PausedAdmin
	}
	if err := saveBox(ctx, m); err != nil {
		return View{}, err
	}
	m.SecretSealed = ""
	return viewOf(&m), nil
}

// Remove disconnects a project's mailbox: soft-deleted, its sealed secret
// wiped in the same statement, and never polled again.
func Remove(ctx context.Context, who *userModels.UserInfo, s workAccess.Standing, projectID uuid.UUID) error {
	if err := manages(ctx, s); err != nil {
		return err
	}
	removed, err := removeBox(ctx, projectID, who.UserPostgresInfo.Id)
	if err != nil {
		return err
	}
	if !removed {
		return ErrNoMailbox
	}
	return nil
}

// Test logs in to a mailbox and opens its folder (nothing fetched, nothing
// kept): what an admin checks before saving. An empty password tests the one
// kept. On the demo it never dials.
func Test(ctx context.Context, who *userModels.UserInfo, s workAccess.Standing, projectID uuid.UUID, in Settings) error {
	if err := manages(ctx, s); err != nil {
		return err
	}
	b := boxOf(who, in)
	if err := checkBox(b); err != nil {
		return err
	}
	if b.Password == "" && !helpers.DemoMode() {
		have, err := byProject(ctx, projectID)
		if err != nil {
			return err
		}
		if !keepsPassword(have, b) {
			return ErrNoPassword
		}
		if b.Password, err = helpers.DecryptSecret(have.SecretSealed); err != nil {
			return ErrNoPassword
		}
	}
	tctx, cancel := context.WithTimeout(ctx, 2*dialTimeout+commandTimeout)
	defer cancel()
	return testBox(tctx, b)
}

// keepsPassword reports whether an empty password may stand for the one kept
// for have: only on the server it was given for (host, port, TLS mode and
// user all unchanged), or an admin could send it to a server they own and read
// it in LOGIN. The folder may change. SaveMailbox's CASE holds the same rule.
func keepsPassword(have *mailModel.Mailbox, b Box) bool {
	return have != nil && have.SecretSealed != "" && have.Host == b.Host && have.Port == b.Port && have.TLSMode == b.TLS && have.Username == b.User
}

// ConnectDemo connects the demo's stub mailbox to a project, as by (the demo
// seed; helpers.DemoMode only): no password, nothing dialled, polled like any
// mailbox, delivering the stub's three messages on its second poll.
func ConnectDemo(ctx context.Context, projectID, by uuid.UUID) error {
	if !helpers.DemoMode() {
		return errors.New("triage mail: the stub mailbox is the demo's only")
	}
	return saveBox(ctx, mailModel.Mailbox{ID: uuid.New(), ProjectUUID: projectID, Host: "demo.invalid", Port: 993, TLSMode: "tls",
		Username: "help@demo.invalid", Folder: "INBOX", ConnectedBy: by})
}
