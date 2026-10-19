//go:build integration
// +build integration

package integration_test

// A database restored from a backup, beside the conversations index (chat A9;
// the P1 of 17 Oct 2026). make backup is taken online: Postgres first, the
// graph after it. Restored, the index and the "clean" of its last comparison
// come back from the first moment beside a graph from the second, so someone
// taken out of a private channel between the two is in the index and out of
// the graph. They are refused until a comparison has run; once one since the
// restore finds the index clean, decisions read it again.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestARestore|TestAServerStartedAfterARestore' -v

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	conversationDomain "github.com/akashc777/OneCamp/domain/Conversation"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
	conversationModels "github.com/akashc777/OneCamp/models/postgres/Conversation"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// restoreWorld is Maya's private channel with Sam in it, its index filled in
// and compared clean, so decisions read the index.
type restoreWorld struct {
	*indexWorld
	ch, chUID string
	maya, sam *userModels.UserInfo
}

func newRestoreWorld(t *testing.T) *restoreWorld {
	t.Helper()
	ctx := context.Background()
	w := &restoreWorld{indexWorld: newIndexWorld(t, "maya", "sam")}
	w.maya, w.sam = w.info(t, "maya"), w.info(t, "sam")
	err, channel := channelBusiness.CreateChannel(ctx, &channelAdapter.InputCreateChannel{ChannelName: "board-pay", ChannelPrivate: true}, w.maya)
	if err != nil {
		t.Fatalf("making the channel: %v", err)
	}
	w.ch = channel.String()
	if err := channelBusiness.AddChannelMemberEdge(ctx, channel, &w.sam.UserDgraphInfo, w.people["sam"]); err != nil {
		t.Fatalf("adding Sam: %v", err)
	}
	var found struct {
		Q []struct {
			UID string `json:"uid"`
		} `json:"q"`
	}
	_ = json.Unmarshal(w.dg.Query(t, `{ q(func: eq(ch_uuid, "`+w.ch+`")) { uid } }`), &found)
	if len(found.Q) != 1 {
		t.Fatalf("the channel's node: %+v", found)
	}
	w.chUID = found.Q[0].UID
	if err := configModel.UpsertConfig(conversationModels.BackfillKey, "done: filled in by the test"); err != nil {
		t.Fatal(err)
	}
	pastTheMargin(t, w.env)
	from, _ := conversationDomain.StateNow(ctx)
	if rep, err := conversationBusiness.Reconcile(ctx, from); err != nil || rep.Repaired != 0 || !rep.Settled {
		t.Fatalf("the comparison before the backup: %s (%v)", rep, err)
	}
	if c := conversationDomain.AccessSource(ctx); !c.Index {
		t.Fatalf("a clean index isn't read: %s", c.Why)
	}
	return w
}

// decides is what who gets in the channel: "refused", or the level.
func (w *restoreWorld) decides(t *testing.T, who *userModels.UserInfo) string {
	t.Helper()
	d, err := conversationBusiness.Resolve(context.Background(), who, conversationBusiness.Ref{Kind: conversationBusiness.KindChannel, ID: w.ch})
	if err != nil {
		if strings.Contains(err.Error(), "no such conversation") {
			return "refused"
		}
		t.Fatalf("deciding: %v", err)
	}
	return fmt.Sprint(d.Level)
}

// backUpPostgres keeps what pg_dumpall would hold of the index: its rows, and
// its state, filled-in mark and asked-for comparison in system_configs.
// restorePostgres puts them back, as replaying that dump does.
func (w *restoreWorld) backUpPostgres(t *testing.T) {
	t.Helper()
	for _, q := range []string{
		`CREATE TABLE backup_members AS SELECT * FROM conversation_members`,
		`CREATE TABLE backup_configs AS SELECT * FROM system_configs WHERE key LIKE 'conversations%'`,
	} {
		if _, err := w.env.PG.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func (w *restoreWorld) restorePostgres(t *testing.T) {
	t.Helper()
	for _, q := range []string{
		`DELETE FROM conversation_members`,
		`INSERT INTO conversation_members SELECT * FROM backup_members`,
		`DELETE FROM system_configs WHERE key LIKE 'conversations%'`,
		`INSERT INTO system_configs SELECT * FROM backup_configs`,
	} {
		if _, err := w.env.PG.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

// takeSamOut takes Sam out of the channel after Postgres was backed up and
// before the graph was: the index first, then the graph, as members.go does.
func (w *restoreWorld) takeSamOut(t *testing.T) {
	t.Helper()
	if err := channelBusiness.DeleteChannelMemberEdge(context.Background(), w.chUID, w.uids["sam"], w.people["sam"].String(), w.ch); err != nil {
		t.Fatalf("taking Sam out: %v", err)
	}
	if got := w.decides(t, w.sam); got != "refused" {
		t.Fatalf("Sam, taken out, gets %q", got)
	}
}

// stateKind is the kind of the index's state now.
func stateKind(t *testing.T) string {
	t.Helper()
	v, err := conversationDomain.StateNow(context.Background())
	s, ok := conversationDomain.ParseState(v)
	if err != nil || !ok {
		t.Fatalf("the state %q: %v", v, err)
	}
	return s.Kind
}

// comparedTwice compares the index with the graph twice, as the job does at a
// worker's start and at its next tick: the first comparison repairs Sam's
// place, which leaves the index unchecked, and the second finds nothing, so
// decisions read the index again, and it refuses Sam.
func (w *restoreWorld) comparedTwice(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	from, _ := conversationDomain.StateNow(ctx)
	if rep, err := conversationBusiness.Reconcile(ctx, from); err != nil || rep.Repaired != 1 || !rep.Settled {
		t.Fatalf("the first comparison after the restore: %s (%v); want Sam's place repaired", rep, err)
	}
	if k := stateKind(t); k != conversationDomain.StateDrift {
		t.Fatalf("the first comparison after the restore left the index %s, want drift", k)
	}
	sameList(t, "the repaired index", w.members(t, w.ch), "maya:manager", "sam:out")
	if got := w.decides(t, w.sam); got != "refused" {
		t.Fatalf("Sam gets %q after the first comparison", got)
	}
	from, _ = conversationDomain.StateNow(ctx)
	if rep, err := conversationBusiness.Reconcile(ctx, from); err != nil || rep.Repaired != 0 || !rep.Settled {
		t.Fatalf("the second comparison: %s (%v)", rep, err)
	}
	if c := conversationDomain.AccessSource(ctx); !c.Index {
		t.Fatalf("a comparison that found nothing didn't bring decisions back to the index: %s", c.Why)
	}
	if got := w.decides(t, w.sam); got != "refused" {
		t.Errorf("the index lets Sam in: %q", got)
	}
	if got := w.decides(t, w.maya); got == "refused" {
		t.Error("the index refuses Maya, who made the channel")
	}
}

// restoredIndexState is the statement make restore runs (Makefile-distribute,
// RESTORED_INDEX_STATE), checked to run once the database is restored and
// before the graph is loaded or anything starts.
func restoredIndexState(t *testing.T) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	b, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "Makefile-distribute"))
	if err != nil {
		t.Fatal(err)
	}
	mk := string(b)
	const def = "\nRESTORED_INDEX_STATE = "
	i := strings.Index(mk, def)
	if i < 0 {
		t.Fatal("make restore doesn't mark the conversations index unchecked: Makefile-distribute has no RESTORED_INDEX_STATE")
	}
	stmt, _, _ := strings.Cut(mk[i+len(def):], "\n")
	j := strings.Index(mk, "\nrestore:")
	if j < 0 {
		t.Fatal("Makefile-distribute has no restore target")
	}
	var recipe strings.Builder
	for _, line := range strings.Split(mk[j+1:], "\n")[1:] {
		if !strings.HasPrefix(line, "\t") {
			break
		}
		recipe.WriteString(line + "\n")
	}
	r := recipe.String()
	at := func(s string) int {
		k := strings.Index(r, s)
		if k < 0 {
			t.Fatalf("the restore recipe has no %q", s)
		}
		return k
	}
	marked, restored := at(`-c "$(RESTORED_INDEX_STATE)"`), at("ok    database restored")
	if !(restored < marked && marked < at("4/5  dgraph") && marked < at("$(COMPOSE) up -d")) {
		t.Fatal("make restore marks the conversations index somewhere other than between Postgres coming back and anything starting")
	}
	return stmt
}

// make restore marks the index unchecked once Postgres is back, so even a
// process that started before the backup's comparison, which it would trust,
// reads the graph until the index has been compared again.
func TestARestoreMarksTheConversationsIndexUnchecked(t *testing.T) {
	ctx := context.Background()
	w := newRestoreWorld(t)
	// A process started an hour before the comparison the backup was taken
	// after: its own start can't make it doubt that comparison.
	t.Cleanup(conversationDomain.StartedForTest(time.Now().Add(-time.Hour)))
	// The backup's rows were written before a comparison's margin, as all but
	// the newest of a real backup's are.
	if _, err := w.env.PG.Exec(`UPDATE conversation_members SET updated_at = NOW() - INTERVAL '1 hour'`); err != nil {
		t.Fatal(err)
	}
	w.backUpPostgres(t)
	w.takeSamOut(t)
	w.restorePostgres(t)
	sameList(t, "the restored index", w.members(t, w.ch), "maya:manager", "sam:member")

	// Without the mark, that process lets Sam in: the restored index has him,
	// and the restored comparison found it clean.
	if got := w.decides(t, w.sam); got == "refused" {
		t.Fatal("the restored index doesn't let Sam in, so this test can't tell the mark matters")
	}
	if _, err := w.env.PG.Exec(restoredIndexState(t)); err != nil {
		t.Fatalf("make restore's statement: %v", err)
	}
	// A process that kept running reads the state again within
	// sourceRecheck; this one does now.
	t.Cleanup(conversationDomain.StartedForTest(time.Now().Add(-time.Hour)))
	v, _ := conversationDomain.StateNow(ctx)
	if s, ok := conversationDomain.ParseState(v); !ok || s.Kind != conversationDomain.StateDirty || s.Note != "restored from a backup" || time.Since(s.At) > time.Minute {
		t.Fatalf("make restore's statement left the state %q", v)
	}
	if c := conversationDomain.AccessSource(ctx); c.Index || !strings.Contains(c.Why, "restored from a backup") {
		t.Fatalf("decisions read an index restored beside a newer graph: %+v", c)
	}
	if got := w.decides(t, w.sam); got != "refused" {
		t.Fatalf("Sam, taken out before the graph was backed up, gets %q after the restore", got)
	}
	w.comparedTwice(t)
}

// A database put back by other means carries no mark. A server started after
// it trusts no comparison from before its start: it refuses Sam, asks for a
// comparison, and reads the index again once one since its start finds it
// clean. Sam's row was written moments before the backup, inside the
// comparison's margin, and is compared all the same: it is from before the
// start.
func TestAServerStartedAfterARestoreDoesntTrustTheComparisonItBroughtBack(t *testing.T) {
	ctx := context.Background()
	w := newRestoreWorld(t)
	// Sam joined moments before the backup.
	if _, err := w.env.PG.Exec(`UPDATE conversation_members SET updated_at = NOW() WHERE user_id = $1`, w.people["sam"]); err != nil {
		t.Fatal(err)
	}
	w.backUpPostgres(t)
	w.takeSamOut(t)
	w.restorePostgres(t)
	sameList(t, "the restored index", w.members(t, w.ch), "maya:manager", "sam:member")
	// A process that started before the backup's comparison lets Sam in: the
	// restored index has him, and nothing says it is older than the graph.
	if got := w.decides(t, w.sam); got == "refused" {
		t.Fatal("the restored index doesn't let Sam in, so this test can't tell the start matters")
	}

	// The server starts in a later second than the comparison the backup was
	// taken after (stamps are to the second).
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	start := time.Now().Truncate(time.Second)
	t.Cleanup(conversationDomain.StartedForTest(start))

	c := conversationDomain.AccessSource(ctx)
	if c.Index || !strings.Contains(c.Why, "before this process started") {
		t.Fatalf("a server started after a restore trusts the comparison it brought back: %+v", c)
	}
	if got := w.decides(t, w.sam); got != "refused" {
		t.Fatalf("Sam, taken out before the graph was backed up, gets %q after the restore", got)
	}
	var asked string
	if err := w.env.PG.QueryRow(`SELECT value FROM system_configs WHERE key = $1`, conversationModels.CompareAfterKey).Scan(&asked); err != nil ||
		asked != start.UTC().Format(time.RFC3339) {
		t.Fatalf("the comparison asked for: %q (%v), want one after %s", asked, err, start.UTC().Format(time.RFC3339))
	}
	w.comparedTwice(t)
}
