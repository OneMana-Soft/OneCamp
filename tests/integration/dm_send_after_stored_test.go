//go:build integration
// +build integration

package integration_test

// Is a message that was stored also sent?
//
// A DM or group message is stored, then the conversation's read marks are
// written. A failed mark returned failure: the sender saw "Not sent", nothing
// else happened (no live message, no push, no search, no notification row
// for a first DM, so no push for its later ones either), and Try again
// stored the message twice. Now a failed mark is logged and the send goes on.
//
// Run: go test -tags=integration ./tests/integration/ -run TestADMStored -v

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	chatNotificationBusiness "github.com/akashc777/OneCamp/business/UserChatNotification"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestADMStoredIsSentEvenWhenItsReadMarksCantBeWritten(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	// Messages are indexed in the background; give them somewhere to go.
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	maya, jonas := uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"maya": maya, "jonas": jonas} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:maya", "user_uuid": maya.String(), "user_name": "maya"},
		{"uid": "_:jonas", "user_uuid": jonas.String(), "user_name": "jonas"},
	})

	// The read marks can't be written.
	if _, err := env.PG.Exec(`ALTER TABLE last_seen_chat RENAME TO last_seen_chat_unwritable`); err != nil {
		t.Fatal(err)
	}

	var sender userModels.UserInfo
	sender.UserPostgresInfo.Id = maya
	sender.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids["maya"], Uuid: maya.String(), UserName: "maya"}
	to := &dgraphStruct.DgraphUser{Uid: uids["jonas"], Uuid: jonas.String(), UserName: "jonas"}

	// Maya's first DM to Jonas.
	created, err := chatBusiness.CreateChat(ctx, &chatAdapter.ChatInfo{TextHtml: "<p>the numbers for Q4</p>", ToUuid: jonas.String()}, &sender, to, jonas, nil)
	if err != nil || created == nil || created.Uuid == "" {
		t.Fatalf("a stored message was answered as not sent: %+v %v", created, err)
	}
	// And sent on: Jonas is notified of it (a first DM writes his
	// notification row, which every later push depends on).
	grouping := helpers.GetGroupingId(maya.String(), jonas.String())
	eligible, err := chatNotificationBusiness.GetEligibleUsersForChatActivity(ctx, grouping, maya.String(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(eligible, jonas.String()) {
		t.Fatalf("Jonas isn't told of Maya's DM: eligible %v", eligible)
	}
	var stored int
	if err := env.PG.QueryRow(`SELECT count(*) FROM chats WHERE id = $1`, created.Uuid).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("the message is stored %d times (%v), want once", stored, err)
	}
}
