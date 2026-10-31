package business

// A member brings their own Slack DMs (chat item 2, the move from Slack). An
// admin's import brings the team's public channels, and a token reads only
// the DMs, group DMs and private channels of the person whose token it is. So
// each person brings their own, with their own token, into the team's import.
//
// What keeps it theirs:
//   - The token is a person's (xoxp-), and that person is the member: the
//     team's import mapped the Slack person to them, or, where it didn't,
//     Slack gives the person's email and it is the member's. An app's token,
//     or anyone else's, is refused before anything is read.
//   - It reads DMs, group DMs and private channels only, and only those the
//     person is in (MembersOnly), which is all Slack gives a person's token.
//   - It runs into the team's import (the same workspace label), whose map
//     brings a conversation two people both bring only once. Each
//     conversation keeps exactly its Slack members, as any import's do.
//   - It is planned and run as soon as it is read, with no admin's review: it
//     holds nothing that isn't the member's own. The admins see that it
//     happened (the audit log and the import history: who, when, how many
//     conversations), never what's in it: no digest is written of it, and its
//     plan and notes are shown to its member alone.
//   - The token is held in memory for the read alone, never stored or logged.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	adminAuditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	importRun "github.com/akashc777/OneCamp/business/ImportRun"
	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	slackapi "github.com/akashc777/OneCamp/services/Slack"
	"github.com/google/uuid"
)

// The refusals of a member's read, as the member reads them.
var (
	ErrNotAPersonsToken   = errors.New("That isn't a Slack user token. Yours starts xoxp-.")
	ErrBotToken           = errors.New("A bot token reads only the channels its app was added to, never your DMs. Use your own user token: it starts xoxp-.")
	ErrNotYourToken       = errors.New("That token is someone else's. Use your own: it reads only your DMs.")
	ErrNoEmail            = errors.New("Slack didn't say whose email that token has. Add users:read.email to your Slack app, reinstall it, and try again.")
	ErrNoTeamImport       = errors.New("Your team's Slack workspace hasn't been imported yet. Ask an admin to import it first.")
	ErrOtherSlack         = errors.New("That token is for a different Slack workspace from the one your team imported.")
	ErrMemberReadUnderWay = errors.New("You're bringing your Slack DMs already. Wait for that to finish.")
	ErrNotYourRead        = errors.New("That isn't one of your reads.")
)

// memberRefusals are the refusals above by the code the app knows each by.
var memberRefusals = []struct {
	err  error
	code string
}{
	{ErrNotAPersonsToken, "bad_token"}, {ErrBotToken, "bad_token"}, {ErrNotYourToken, "not_your_token"},
	{ErrNoEmail, "no_email"}, {ErrNoTeamImport, "no_team_import"}, {ErrOtherSlack, "other_slack"},
	{ErrMemberReadUnderWay, "under_way"}, {ErrNotYourRead, "not_yours"},
}

// MemberReadRefusal is a member's read refused, as the code the app knows it
// by and a sentence the member can act on: one of the refusals above, or
// Slack's own (refused the token, a missing permission, slow down). ok is
// false for a failure of the server's own, which isn't theirs to fix.
func MemberReadRefusal(err error) (code, msg string, ok bool) {
	for _, r := range memberRefusals {
		if errors.Is(err, r.err) {
			return r.code, r.err.Error(), true
		}
	}
	var se *slackapi.Error
	if errors.As(err, &se) {
		return "slack_refused", apiReadFailure(err, 0), true
	}
	return "", "", false
}

// memberKinds are what a member's read reads: their own conversations.
var memberKinds = []string{"private_channel", "mpim", "im"}

// memberJobOptions marks a job as a member's own read. It keeps the
// conversations' joins and topics, as an admin's import does by default.
var memberJobOptions = json.RawMessage(`{"member_dms": true, "skip_subtypes": false, "send_invites": false}`)

// StartMemberRead checks that token is member's own, finds the team's import
// it belongs in, and starts reading the member's DMs, group DMs and private
// channels into it, to be planned and run when the read ends. It answers the
// job; a refusal is one of the errors above, or Slack's own, as a sentence.
func StartMemberRead(ctx context.Context, member *userModels.UserInfo, token string, opts APIReadOptions) (uuid.UUID, error) {
	token = strings.TrimSpace(token)
	if strings.HasPrefix(token, "xoxb-") {
		return uuid.Nil, ErrBotToken
	}
	if !strings.HasPrefix(token, "xoxp-") {
		return uuid.Nil, ErrNotAPersonsToken
	}
	c := newAPIClient(token)
	who, err := tokenOwner(ctx, c)
	if err != nil {
		return uuid.Nil, err
	}
	if who.Bot {
		return uuid.Nil, ErrBotToken
	}
	workspace, err := workspaceOfTeam(ctx, who.TeamID)
	if err != nil {
		return uuid.Nil, err
	}
	if err := isTheMember(ctx, c, workspace, who.UserID, member); err != nil {
		return uuid.Nil, err
	}
	busy, err := importModels.MemberReadUnderWay(ctx, member.UserPostgresInfo.Id)
	if err != nil {
		return uuid.Nil, err
	}
	if busy {
		return uuid.Nil, ErrMemberReadUnderWay
	}
	opts.Kinds, opts.MembersOnly = memberKinds, true
	return StartAPIRead(ctx, APIRead{
		Workspace:  workspace,
		Token:      token,
		Options:    opts,
		JobOptions: memberJobOptions,
		Then:       planAndRunMemberRead(member),
	}, member.UserPostgresInfo.Id)
}

// isTheMember checks that the token's Slack person is the member: the team's
// import mapped them to the member, or, for a person it didn't map, Slack
// gives their email and it is the member's.
func isTheMember(ctx context.Context, c apiClient, workspace, slackUser string, member *userModels.UserInfo) error {
	mapped, err := importModels.LookupWorkspaceMapping(ctx, workspace, importModels.EntityUser, slackUser)
	if err == nil && mapped != uuid.Nil {
		if mapped == member.UserPostgresInfo.Id {
			return nil
		}
		return ErrNotYourToken
	}
	var info struct {
		slackapi.OK
		User struct {
			Profile struct {
				Email string `json:"email"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := c.Call(ctx, "users.info", url.Values{"user": {slackUser}}, &info); err != nil {
		return fmt.Errorf("Slack wouldn't say whose token it is: %w", err)
	}
	if strings.TrimSpace(info.User.Profile.Email) == "" {
		return ErrNoEmail
	}
	if sameEmail(info.User.Profile.Email, member.UserPostgresInfo.EmailID) {
		return nil
	}
	return ErrNotYourToken
}

// sameEmail is whether two addresses are one, ignoring case and space; an
// empty one is nobody's. Pure.
func sameEmail(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	return a != "" && strings.EqualFold(a, b)
}

// workspaceOfTeam is the label of the team's own import of Slack workspace
// team: its newest completed import by an admin. One made before imports
// noted their team counts when it is the only Slack workspace imported.
func workspaceOfTeam(ctx context.Context, team string) (string, error) {
	imported, err := importModels.SlackWorkspaces(ctx)
	if err != nil {
		return "", err
	}
	return pickWorkspace(imported, team)
}

// pickWorkspace is workspaceOfTeam's choice among the imports: the one of
// team, else the only workspace imported if it noted no team. Pure.
func pickWorkspace(imported []importModels.SlackWorkspace, team string) (string, error) {
	if len(imported) == 0 {
		return "", ErrNoTeamImport
	}
	labels := map[string]bool{}
	for _, w := range imported {
		if team != "" && w.Team == team {
			return w.Label, nil
		}
		labels[w.Label] = true
	}
	if len(labels) == 1 && imported[0].Team == "" {
		return imported[0].Label, nil
	}
	return "", ErrOtherSlack
}

// planAndRunMemberRead is what follows a member's read: the record that it
// happened, then its plan and its run, as that member.
func planAndRunMemberRead(member *userModels.UserInfo) func(ctx context.Context, jobId uuid.UUID, report APIReadReport) {
	return func(ctx context.Context, jobId uuid.UUID, report APIReadReport) {
		recordMemberRead(ctx, member, jobId, report)
		fail := func(why string, err error) {
			helpers.LogWarnWithContext(ctx, "SlackImport member read job=%s: %s: %+v", jobId, why, err)
			msg := why
			_, _ = importModels.UpdateStatusFrom(ctx, jobId, []string{importModels.StatusValidating, importModels.StatusPlanned},
				importModels.StatusFailed, strPtr("failed"), &msg)
		}
		arc, done, err := OpenStagedZipFromMinIO(ctx, StagedExportKey(jobId))
		if err != nil {
			fail("Couldn't open what was read from Slack. Bring your DMs again.", err)
			return
		}
		_, err = BuildPlan(ctx, jobId, arc)
		done()
		if err != nil {
			fail("Couldn't plan what was read from Slack. Bring your DMs again.", err)
			return
		}
		lease, err := importRun.Take(jobId)
		if err != nil {
			fail("Couldn't start bringing your DMs in. Bring them again.", err)
			return
		}
		defer lease.Release()
		if _, err := lease.Start(ctx, []string{importModels.StatusPlanned},
			func() { RunImport(context.Background(), jobId, member) }); err != nil {
			fail("Couldn't start bringing your DMs in. Bring them again.", err)
		}
	}
}

// recordMemberRead puts the read in the audit log: who, which import, and how
// many conversations of each kind. Never a conversation's name or words.
func recordMemberRead(ctx context.Context, member *userModels.UserInfo, jobId uuid.UUID, r APIReadReport) {
	id := member.UserPostgresInfo.Id
	n := r.DMs + r.MPIMs + r.Groups
	err := adminAuditBusiness.RecordForPrincipal(ctx, &id, member.UserPostgresInfo.EmailID, adminAuditBusiness.ActorHuman,
		"slack_import.member_read", adminAuditBusiness.CategoryIntegration,
		fmt.Sprintf("Brought %d of their own Slack conversations into the team's import", n),
		map[string]interface{}{"job_id": jobId.String(), "dms": r.DMs, "group_dms": r.MPIMs, "private_channels": r.Groups})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "SlackImport member read job=%s: the audit record: %+v", jobId, err)
	}
}

// MemberReads is member's own reads of their Slack conversations, newest
// first, and whether their team has imported a Slack workspace their reads
// can go into: what their settings show.
func MemberReads(ctx context.Context, member uuid.UUID) (reads []*importModels.Job, teamImported bool, err error) {
	if reads, err = importModels.ListMemberReads(ctx, member, 10); err != nil {
		return nil, false, err
	}
	imported, err := importModels.SlackWorkspaces(ctx)
	return reads, len(imported) > 0, err
}

// memberReadOf is the job jobId when it is member's own read, else
// ErrNotYourRead: another's job is answered as if it weren't there.
func memberReadOf(ctx context.Context, member, jobId uuid.UUID) (*importModels.Job, error) {
	job, err := importModels.GetJob(ctx, jobId)
	if errors.Is(err, importModels.ErrJobNotFound) || (err == nil && (job == nil || !MemberRead(job.Options) ||
		job.TriggeredBy == nil || *job.TriggeredBy != member)) {
		return nil, ErrNotYourRead
	}
	return job, err
}

// StopMemberRead stops member's own read: one still reading or waiting is
// discarded, one bringing their conversations in stops where it is, and
// what has come across stays.
func StopMemberRead(ctx context.Context, member, jobId uuid.UUID) error {
	if _, err := memberReadOf(ctx, member, jobId); err != nil {
		return err
	}
	return CancelImport(ctx, jobId)
}

// MemberReadNotes is a page of what member's own read noted: which of their
// conversations or files Slack didn't give, and why.
func MemberReadNotes(ctx context.Context, member, jobId uuid.UUID, limit, offset int) ([]map[string]interface{}, error) {
	if _, err := memberReadOf(ctx, member, jobId); err != nil {
		return nil, err
	}
	return importModels.ListErrors(ctx, jobId, "", limit, offset)
}
