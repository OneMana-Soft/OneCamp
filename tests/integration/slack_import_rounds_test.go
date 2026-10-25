//go:build integration
// +build integration

package integration_test

// A Slack import's statements grow with the messages it brings by what making
// each one takes, and no more (backend audit U1a). Every message also looked
// up its own mapping twice, its author three times, wrote two mapping rows
// and set its own date afterwards: about twice the statements making it took.
//
// Run: go test -tags=integration ./tests/integration/ -run TestASlackImportsStatementsGrowOnlyWithWhatItMakes -v

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	slackBusiness "github.com/akashc777/OneCamp/business/SlackImport"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// slackExportOf is an export of one channel, #launch, where Ada and Bo say
// n things between them, every tenth a thread reply to the one before.
func slackExportOf(t *testing.T, n int) []byte {
	t.Helper()
	var msgs []string
	parent := ""
	for i := 0; i < n; i++ {
		ts := fmt.Sprintf("1759309%03d.%06d", i/1000, i)
		user := []string{"U1", "U2"}[i%2]
		switch {
		case i%10 == 9 && parent != "":
			msgs = append(msgs, fmt.Sprintf(`{"type":"message","user":%q,"text":"reply %d to <@U1>","ts":%q,"thread_ts":%q,"parent_user_id":"U1"}`, user, i, ts, parent))
		default:
			parent = ts
			msgs = append(msgs, fmt.Sprintf(`{"type":"message","user":%q,"text":"message %d for <@U2>","ts":%q}`, user, i, ts))
		}
	}
	var export bytes.Buffer
	zw := zip.NewWriter(&export)
	for name, body := range map[string]string{
		"users.json": `[{"id":"U1","name":"ada","real_name":"Ada","profile":{"email":"ada@acme.test","real_name":"Ada"}},
			{"id":"U2","name":"bo","real_name":"Bo","profile":{"email":"bo@acme.test","real_name":"Bo"}}]`,
		"channels.json": `[{"id":"C1","name":"launch","created":1759300000,"creator":"U1","is_archived":false,"is_general":false,
			"members":["U1","U2"],"topic":{"value":""},"purpose":{"value":""}}]`,
		"launch/2025-10-01.json": "[" + strings.Join(msgs, ",") + "]",
	} {
		f, _ := zw.Create(name)
		_, _ = f.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return export.Bytes()
}

// slackImportStatements runs an import of n messages and answers the
// statements it sent Postgres, having checked they all came across.
func slackImportStatements(t *testing.T, n int) int {
	ctx := context.Background()
	e := newRaceEnv(t, map[string][]byte{"raw.zip": slackExportOf(t, n)})
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[]}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	if _, err := e.PG.Exec(`UPDATE users SET username = 'admin', is_admin = true WHERE id = $1`, e.admin); err != nil {
		t.Fatal(err)
	}
	dg.Mutate(t, map[string]any{"dgraph.type": "User", "user_uuid": e.admin.String(), "user_name": "admin"})
	for _, name := range []string{"ada", "bo"} {
		id := uuid.New()
		if _, err := e.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name); err != nil {
			t.Fatal(err)
		}
		dg.Mutate(t, map[string]any{"dgraph.type": "User", "user_uuid": id.String(), "user_name": name, "user_email_id": name + "@acme.test"})
	}
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, e.admin)
	if err != nil || pg == nil {
		t.Fatalf("load the admin: %v", err)
	}
	gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, e.admin.String())
	if err != nil || gr == nil {
		t.Fatalf("load the admin's node: %v", err)
	}
	admin := userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}

	job := e.job(t, "slack", "Acme", "planned", "slackImport/acme/raw.zip")
	arc, done, err := slackBusiness.OpenStagedZipFromMinIO(ctx, "slackImport/acme/raw.zip")
	if err != nil {
		t.Fatalf("open the export: %v", err)
	}
	if _, err := slackBusiness.BuildPlan(ctx, job, arc); err != nil {
		t.Fatalf("plan: %v", err)
	}
	done()

	counter := integration.CountPostgres(t, e.Env)
	slackBusiness.RunImport(counter.Context(ctx), job, &admin)
	sent := counter.Count()

	var posts, comments int
	if err := e.PG.QueryRow(`SELECT (SELECT count(*) FROM posts), (SELECT count(*) FROM comments)`).Scan(&posts, &comments); err != nil {
		t.Fatal(err)
	}
	if posts+comments != n {
		var failures []string
		rows, _ := e.PG.Query(`SELECT code || ': ' || message FROM import_errors WHERE import_id = $1 LIMIT 5`, job)
		for rows != nil && rows.Next() {
			var f string
			_ = rows.Scan(&f)
			failures = append(failures, f)
		}
		var status, stage, msg string
		_ = e.PG.QueryRow(`SELECT status, COALESCE(stage, ''), COALESCE(error_message, '') FROM import_jobs WHERE id = $1`, job).Scan(&status, &stage, &msg)
		t.Fatalf("%d messages and %d replies came across of %d (job %s at %s: %q; errors %v)", posts, comments, n, status, stage, msg, failures)
	}
	return sent
}

func TestASlackImportsStatementsGrowOnlyWithWhatItMakes(t *testing.T) {
	small, large := 50, 500
	var few, many int
	t.Run("few", func(t *testing.T) { few = slackImportStatements(t, small) })
	t.Run("many", func(t *testing.T) { many = slackImportStatements(t, large) })
	if t.Failed() {
		return
	}
	perMessage := float64(many-few) / float64(large-small)
	t.Logf("statements: %d for %d messages, %d for %d: %.2f a message", few, small, many, large, perMessage)
	limit := slackImportStatementsPerMessage
	if v := os.Getenv("SLACK_IMPORT_PER_MESSAGE"); v != "" {
		fmt.Sscan(v, &limit)
	}
	if perMessage > limit {
		t.Errorf("an imported message costs %.2f statements, more than the %.2f making it takes", perMessage, limit)
	}
}

// slackImportStatementsPerMessage is what making a message takes in
// Postgres, its row and its one mapping row, with a little room for the
// replies (every tenth here), whose thread is read too. Measured at 2.13.
const slackImportStatementsPerMessage = 2.25
