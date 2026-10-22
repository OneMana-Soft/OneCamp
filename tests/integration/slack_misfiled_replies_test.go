//go:build integration
// +build integration

package integration_test

// Replies an earlier import filed in another conversation's thread are put
// right (v2.75.1, business/SlackImport repair.go). Before a message was known
// by its conversation and its ts, #hiring's reply under its interview notes
// could be filed under #general's message at the same ts, for everyone in
// #general to read, and the notes themselves were left unmapped. Each test
// builds that state as the old import left it, from the colliding export
// (slack_message_identity_test.go), and runs the repair.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestAReply|TestAMisfiled' -v

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	slackBusiness "github.com/akashc777/OneCamp/business/SlackImport"
	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

// misfiledWorld is the colliding export imported, then left as the old import
// left it: #hiring's reply filed under #general's message instead of its own,
// the notes it answers unmapped, and the import not yet checked.
type misfiledWorld struct {
	*acmeWorld
	job               uuid.UUID
	general, notes    uuid.UUID // #general's message and #hiring's, at one ts
	misfiled          uuid.UUID // the reply, under #general's message
	generalCh, hiring string    // the channels' uuids
}

func newMisfiledWorld(t *testing.T) *misfiledWorld {
	t.Helper()
	ctx := context.Background()
	w := newAcmeWorld(t, map[string][]byte{"raw.zip": zipTree(t, collidingExport(t))})
	job := w.e.job(t, "slack", "Acme", "planned", "slackImport/acme/raw.zip")
	arc, done, err := slackBusiness.OpenStagedZipFromMinIO(ctx, "slackImport/acme/raw.zip")
	if err != nil {
		t.Fatalf("open the export: %v", err)
	}
	if _, err := slackBusiness.BuildPlan(ctx, job, arc); err != nil {
		t.Fatalf("plan: %v", err)
	}
	done()
	slackBusiness.RunImport(ctx, job, &w.admin)
	if status, _ := w.e.state(t, job); status != "completed" {
		t.Fatalf("the import ended %q", status)
	}
	mapped := func(entity, key string) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := w.e.PG.QueryRow(`SELECT onecamp_uuid FROM import_id_map WHERE import_id = $1 AND entity_type = $2 AND source_id = $3`, job, entity, key).Scan(&id); err != nil {
			t.Fatalf("%s %s: %v", entity, key, err)
		}
		return id
	}
	m := &misfiledWorld{acmeWorld: w, job: job,
		general: mapped("message", "C0GENERAL1/"+collidingTs), notes: mapped("message", "G0HIRING1/"+collidingTs),
		generalCh: mapped("channel", "C0GENERAL1").String(), hiring: mapped("channel", "G0HIRING1").String()}
	reply := mapped("comment", "G0HIRING1/"+replyTs)

	// The old import: the reply under #general's message, as Ada, at its time.
	ada := signedIn(t, w.people["ada"])
	post, err := postBusiness.GetUnDeletedDgraphPostChannelBasicInfo(ctx, m.general.String())
	if err != nil || post == nil {
		t.Fatalf("#general's message: %v", err)
	}
	at := time.Unix(1759310100, 200000).UTC()
	created, err := postBusiness.CreatePostComment(helpers.WithImportTimestamp(ctx, at), &postAdapter.InputCreateOrUpdateCommentToPost{
		HTMLText: "<p>Candidate two is strong</p>", PostUuid: m.general.String(),
	}, ada, nil, post)
	if err != nil {
		t.Fatalf("file the reply under #general: %v", err)
	}
	m.misfiled = uuid.MustParse(created.Uuid)
	c, err := commentBusiness.GetDgraphPostCommentInfoByUUID(ctx, reply.String(), "")
	if err != nil || c == nil {
		t.Fatalf("the reply where it belongs: %v", err)
	}
	if err := postBusiness.DeleteCommentOnPost(ctx, reply, c, ada.UserPostgresInfo.Id.String()); err != nil {
		t.Fatalf("take the reply from its own thread: %v", err)
	}
	// Its rows as the old import and the key migration left them: the notes
	// unmapped (#general's message held the ts first), the reply keyed under
	// #general's.
	for _, q := range []string{
		`DELETE FROM import_id_map WHERE import_id = $1 AND entity_type = 'message' AND source_id = 'G0HIRING1/` + collidingTs + `'`,
		`DELETE FROM import_id_map WHERE import_id = $1 AND entity_type = 'comment'`,
		`DELETE FROM import_workspace_id_map WHERE origin_import_id = $1 AND entity_type IN ('message', 'comment') AND source_id LIKE 'G0HIRING1/%'`,
		`UPDATE import_jobs SET replies_checked_at = NULL, completed_at = NOW() - INTERVAL '30 days' WHERE id = $1`,
	} {
		if _, err := w.e.PG.Exec(q, job); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.e.PG.Exec(`INSERT INTO import_id_map (import_id, entity_type, source_id, onecamp_uuid, parent_source_id, metadata)
		VALUES ($1, 'comment', $2, $3, $4, '{"thread_ts": "`+collidingTs+`"}')`,
		job, "C0GENERAL1/"+replyTs, m.misfiled, "C0GENERAL1/"+collidingTs); err != nil {
		t.Fatal(err)
	}
	return m
}

// threads is each channel's live messages with their live replies, as
// "#channel: message / reply by author".
func (m *misfiledWorld) threads(t *testing.T) []string {
	t.Helper()
	var graph struct {
		Channels []struct {
			Name  string `json:"ch_name"`
			Posts []struct {
				Text     string     `json:"post_text"`
				Deleted  *time.Time `json:"post_deleted_at"`
				Comments []struct {
					Text    string     `json:"comment_text"`
					Deleted *time.Time `json:"comment_deleted_at"`
					By      struct {
						Name string `json:"user_name"`
					} `json:"comment_by"`
				} `json:"post_comments"`
			} `json:"ch_posts"`
		} `json:"channels"`
	}
	raw := m.dg.Query(t, `{ channels(func: has(ch_uuid)) { ch_name ch_posts { post_text post_deleted_at
		post_comments { comment_text comment_deleted_at comment_by { user_name } } } } }`)
	if err := json.Unmarshal(raw, &graph); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	var out []string
	for _, c := range graph.Channels {
		for _, p := range c.Posts {
			if helpers.IsSoftDeleted(p.Deleted) {
				continue
			}
			line := "#" + c.Name + ": " + helpersText(p.Text)
			for _, r := range p.Comments {
				if !helpers.IsSoftDeleted(r.Deleted) {
					line += " / " + helpersText(r.Text) + " by " + r.By.Name
				}
			}
			out = append(out, line)
		}
	}
	sort.Strings(out)
	return out
}

// audit is the repair's audit record's summary and counts, "" for none.
func (m *misfiledWorld) audit(t *testing.T) (string, string) {
	t.Helper()
	var summary, meta string
	err := m.e.PG.QueryRow(`SELECT summary, metadata::text FROM admin_audit_log WHERE action = 'slack_import.replies_repaired' ORDER BY created_at DESC LIMIT 1`).Scan(&summary, &meta)
	if err != nil {
		return "", ""
	}
	return summary, meta
}

func (m *misfiledWorld) checked(t *testing.T) bool {
	t.Helper()
	var at *time.Time
	if err := m.e.PG.QueryRow(`SELECT replies_checked_at FROM import_jobs WHERE id = $1`, m.job).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at != nil
}

// The export still kept: the reply goes back under #hiring's notes (found by
// their time, the collision having left them unmapped), and #general holds
// nothing of #hiring. The export waits for the check, then may go. The
// admins are told counts, and a second run changes nothing.
func TestAReplyFiledInAPublicThreadGoesBackToItsPrivateChannel(t *testing.T) {
	ctx := context.Background()
	m := newMisfiledWorld(t)
	before := fmt.Sprint(m.threads(t))
	if !strings.Contains(before, "#general: Plan for Friday / Candidate two is strong by ada") {
		t.Fatalf("the old import's state isn't set up: %s", before)
	}
	held := func() bool {
		t.Helper()
		jobs, err := importModels.ListJobsForRawCleanup(ctx, time.Now().AddDate(0, 0, -7))
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range jobs {
			if j.Id == m.job {
				return false
			}
		}
		return true
	}
	if !held() {
		t.Error("the weekly clean-up would delete the export before the repair read it")
	}

	report, err := slackBusiness.RepairMisfiledReplies(ctx)
	if err != nil {
		t.Fatalf("the repair: %v", err)
	}
	if report.Checked != 1 || report.Moved != 1 || report.TakenOut != 0 || report.ChannelsUnchecked != 0 {
		t.Errorf("the repair's report: %+v, want one import checked and one reply moved", report)
	}
	want := "[#general: Plan for Friday #hiring: Interview notes / Candidate two is strong by ada]"
	if got := fmt.Sprint(m.threads(t)); got != want {
		t.Errorf("after the repair:\n got %s\nwant %s", got, want)
	}
	if !m.checked(t) || held() {
		t.Error("the import isn't marked checked, or its export is still held")
	}
	summary, meta := m.audit(t)
	if !strings.Contains(meta, `"moved": 1`) || strings.Contains(summary+meta, "Candidate") || strings.Contains(summary+meta, "hiring") {
		t.Errorf("the admins' record: %q %s", summary, meta)
	}

	// Again, and again from unchecked: nothing more moves.
	if again, err := slackBusiness.RepairMisfiledReplies(ctx); err != nil || again.Checked != 0 {
		t.Errorf("a second run: %+v %v", again, err)
	}
	if _, err := m.e.PG.Exec(`UPDATE import_jobs SET replies_checked_at = NULL WHERE id = $1`, m.job); err != nil {
		t.Fatal(err)
	}
	if again, err := slackBusiness.RepairMisfiledReplies(ctx); err != nil || again.Moved+again.TakenOut != 0 {
		t.Errorf("a run over a repaired import: %+v %v", again, err)
	}
	if got := fmt.Sprint(m.threads(t)); got != want {
		t.Errorf("after running again:\n got %s\nwant %s", got, want)
	}
}

// The export kept, but the reply's own message gone: it has nowhere to go,
// and is taken out of #general.
func TestAMisfiledReplyWithNoHomeIsTakenOut(t *testing.T) {
	ctx := context.Background()
	m := newMisfiledWorld(t)
	if _, err := m.e.PG.Exec(`UPDATE posts SET deleted_at = NOW() WHERE id = $1`, m.notes); err != nil {
		t.Fatal(err)
	}
	report, err := slackBusiness.RepairMisfiledReplies(ctx)
	if err != nil || report.Moved != 0 || report.TakenOut != 1 {
		t.Fatalf("the repair: %+v %v, want one reply taken out", report, err)
	}
	for _, line := range m.threads(t) {
		if strings.Contains(line, "Candidate two") {
			t.Errorf("the reply is still readable: %s", line)
		}
	}
	var rows int
	if err := m.e.PG.QueryRow(`SELECT count(*) FROM import_id_map WHERE import_id = $1 AND onecamp_uuid = $2`, m.job, m.misfiled).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("the taken-out reply is still mapped (%d, %v): a re-import wouldn't bring it back", rows, err)
	}
}

// The export gone: a DM's reply whose author isn't one of the DM's people is
// taken out, and the import counts as one whose channel replies couldn't be
// checked.
func TestAMisfiledDMReplyIsTakenOutWithoutTheExport(t *testing.T) {
	ctx := context.Background()
	m := newMisfiledWorld(t)
	// Another DM's reply (Cy's, whom the import brought as slack-cy), filed
	// under Ada and Bo's DM message at the colliding ts.
	var dmMessage uuid.UUID
	if err := m.e.PG.QueryRow(`SELECT onecamp_uuid FROM import_id_map WHERE import_id = $1 AND entity_type = 'message' AND source_id = $2`,
		m.job, "D0ADABO01/"+collidingTs).Scan(&dmMessage); err != nil {
		t.Fatal(err)
	}
	var grouping string
	if err := m.e.PG.QueryRow(`SELECT grp_id FROM chats WHERE id = $1`, dmMessage).Scan(&grouping); err != nil {
		t.Fatal(err)
	}
	cy := uuid.New()
	if _, err := m.e.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, 'cy@elsewhere.test', 'cy', 'cy', NOW(), NOW())`, cy); err != nil {
		t.Fatal(err)
	}
	m.dg.Mutate(t, map[string]any{"dgraph.type": "User", "user_uuid": cy.String(), "user_name": "cy"})
	cyInfo := signedIn(t, cy)
	chat, err := chatBusiness.GetDgraphChatByUUID(ctx, dmMessage.String())
	if err != nil || chat == nil {
		t.Fatalf("the DM's message: %v", err)
	}
	created, err := chatBusiness.CreateChatComment(ctx, &chatAdapter.InputCreateOrUpdateCommentToChat{
		HTMLText: "<p>Salary bands attached</p>", ChatUuid: dmMessage.String(),
	}, cyInfo, nil, grouping, cyInfo.UserDgraphInfo.Uid, chat)
	if err != nil {
		t.Fatalf("file Cy's reply in Ada and Bo's DM: %v", err)
	}
	if _, err := m.e.PG.Exec(`INSERT INTO import_id_map (import_id, entity_type, source_id, onecamp_uuid, parent_source_id) VALUES ($1, 'comment', $2, $3, $4)`,
		m.job, "D0ADABO01/1759310200.000300", uuid.MustParse(created.Uuid), "D0ADABO01/"+collidingTs); err != nil {
		t.Fatal(err)
	}
	if _, err := m.e.PG.Exec(`UPDATE import_jobs SET raw_object_key = NULL WHERE id = $1`, m.job); err != nil {
		t.Fatal(err)
	}

	report, err := slackBusiness.RepairMisfiledReplies(ctx)
	if err != nil || report.TakenOut != 1 || report.ChannelsUnchecked != 1 || report.Moved != 0 {
		t.Fatalf("the repair: %+v %v, want Cy's reply taken out and the channels counted unchecked", report, err)
	}
	c, err := commentBusiness.GetDgraphChatCommentInfoByUUID(ctx, created.Uuid, "")
	if err == nil && c != nil && !helpers.IsSoftDeleted(c.DeletedAt) {
		t.Error("Cy's reply is still in Ada and Bo's DM")
	}
	// Without the export, #general's misfiled reply can't be told from its own.
	if !strings.Contains(fmt.Sprint(m.threads(t)), "Candidate two is strong") {
		t.Error("a channel reply was touched without the export to say where it belongs")
	}
	if summary, _ := m.audit(t); !strings.Contains(summary, "couldn't be checked") {
		t.Errorf("the admins aren't told what couldn't be checked: %q", summary)
	}
}
