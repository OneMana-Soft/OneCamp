//go:build integration
// +build integration

package integration_test

// A forward carries, into each place it goes, only the files its sender
// uploaded in the forward box for that place, each with the key its upload
// stored: the files a message lists are served through its place
// (business/FileAccess), so any other entry opened for no one, or for the
// wrong people once a share copied it. And a file uploaded for a group is the
// group's file, so it opens for the group's people.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAForwardCarriesOnlyItsSendersOwnUploadsToEachPlace -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	userAdapter "github.com/akashc777/OneCamp/adapter/User"
	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	fileAccessBusiness "github.com/akashc777/OneCamp/business/FileAccess"
	userController "github.com/akashc777/OneCamp/controllers/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAForwardCarriesOnlyItsSendersOwnUploadsToEachPlace(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
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

	sam, alex, kim := uuid.New(), uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"sam": sam, "alex": alex, "kim": kim} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
	}
	general, private, source := uuid.New(), uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'general', false)`, general); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, source, general, sam); err != nil {
		t.Fatal(err)
	}
	crew, err := helpers.GenerateGroupID([]string{sam.String(), alex.String(), kim.String()})
	if err != nil {
		t.Fatal(err)
	}
	dm := helpers.GetGroupingId(sam.String(), alex.String())
	live := "0001-01-01T00:00:00Z"
	person := func(name string, id uuid.UUID) map[string]any {
		return map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name, "user_deleted_at": live}
	}
	uids := dg.Mutate(t, []map[string]any{
		person("sam", sam), person("alex", alex), person("kim", kim),
		{"uid": "_:general", "dgraph.type": "Channel", "ch_uuid": general.String(), "ch_name": "general",
			"ch_private": false, "ch_deleted_at": live, "ch_members": []map[string]any{{"uid": "_:sam"}},
			"ch_posts": []map[string]any{{"uid": "_:source", "dgraph.type": "Post", "post_uuid": source.String(),
				"post_text": "<p>ship it</p>", "post_by": map[string]any{"uid": "_:sam"}, "post_channel": map[string]any{"uid": "_:general"},
				"post_created_at": "2026-10-01T10:00:00Z", "post_deleted_at": live}}},
		{"uid": "_:crew", "dgraph.type": "Dm", "dm_grouping_id": crew,
			"dm_participants": []map[string]any{{"uid": "_:sam"}, {"uid": "_:alex"}, {"uid": "_:kim"}}},
	})

	// Sam's uploads in the forward box, one for each place (an upload's id is
	// its row's), alex's file of a channel sam isn't in, and an id nobody
	// uploaded.
	upload := func(by uuid.UUID, key string, channels, chats []string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if err := attachmentBusiness.CreateAttachmentForChannelsAndChats(ctx, id, key, by, channels, chats); err != nil {
			t.Fatal(err)
		}
		return id
	}
	inGeneral := upload(sam, sam.String()+"/general.png", []string{general.String()}, nil)
	inCrew := upload(sam, sam.String()+"/crew.png", nil, []string{crew})
	inDM := upload(sam, sam.String()+"/dm.png", nil, []string{dm})
	theirs := upload(alex, alex.String()+"/salaries.xlsx", []string{private.String()}, nil)
	att := func(id uuid.UUID, key string) *dgraphStruct.DgraphAttachment {
		return &dgraphStruct.DgraphAttachment{Uuid: id.String(), ObjectKey: key, FileName: "file"}
	}

	// The group's people open the file uploaded for the group.
	if _, err := fileAccessBusiness.FileOf(ctx, fileAccessBusiness.ConversationOf(crew), inCrew.String()); err != nil {
		t.Errorf("the file uploaded for the group isn't the group's: %v", err)
	}

	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, sam)
	if err != nil || pg == nil {
		t.Fatalf("load sam: %v", err)
	}
	gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, sam.String())
	if err != nil || gr == nil {
		t.Fatalf("load sam's node: %v", err)
	}
	asSam := context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})
	// forward forwards #general's message to the places to with files, and
	// says whether it was written.
	forward := func(to []userAdapter.UserAndChannelFwdMessage, files ...*dgraphStruct.DgraphAttachment) bool {
		t.Helper()
		raw, _ := json.Marshal(userAdapter.UserFwdMsgInput{
			HtmlText: "<p>ship it</p>", ChannelUuid: general.String(), PostUuid: source.String(), FwdTo: to, Attachments: files,
		})
		rec := httptest.NewRecorder()
		userController.FwdUserMessage(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(asSam))
		if rec.Code != http.StatusOK {
			t.Errorf("the forward to %d places: %d %s", len(to), rec.Code, rec.Body.String())
		}
		return rec.Code == http.StatusOK
	}
	// carried is the files the forward's message in a place lists, read from
	// the graph by query.
	carried := func(query string) []string {
		t.Helper()
		resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().Query(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Q []struct {
				Messages []struct {
					Files []struct {
						ID  string `json:"attachment_uuid"`
						Key string `json:"attachment_obj_key"`
					} `json:"files"`
				} `json:"messages"`
			} `json:"q"`
		}
		if err := json.Unmarshal(resp.Json, &out); err != nil || len(out.Q) != 1 || len(out.Q[0].Messages) != 1 {
			t.Fatalf("read back: %v %s", err, resp.Json)
		}
		var got []string
		for _, f := range out.Q[0].Messages[0].Files {
			got = append(got, f.ID+" "+f.Key)
		}
		return got
	}
	const files = `files: %s { attachment_uuid attachment_obj_key }`
	inConversation := func(groupingID string) []string {
		return carried(`{ q(func: eq(dm_grouping_id, "` + groupingID + `")) { messages: dm_chats { ` + fmt.Sprintf(files, "chat_attachments") + ` } } }`)
	}

	// Into the DM alone: sam's upload for it under alex's key, alex's file and
	// an id nobody uploaded.
	if forward([]userAdapter.UserAndChannelFwdMessage{{UserUuid: alex.String()}},
		att(inDM, alex.String()+"/salaries.xlsx"), att(theirs, alex.String()+"/salaries.xlsx"), att(uuid.New(), "made/up.png")) {
		if got, want := inConversation(dm), []string{inDM.String() + " " + sam.String() + "/dm.png"}; !slices.Equal(got, want) {
			t.Errorf("the forward in the DM carries %v; want only sam's upload for it, with its key: %v", got, want)
		}
	}
	// Into #general, then the group, each with sam's uploads for both places
	// and alex's file: each carries only its own upload.
	both := func() []*dgraphStruct.DgraphAttachment {
		return []*dgraphStruct.DgraphAttachment{att(inGeneral, sam.String()+"/general.png"), att(inCrew, sam.String()+"/crew.png"), att(theirs, alex.String()+"/salaries.xlsx")}
	}
	if forward([]userAdapter.UserAndChannelFwdMessage{{ChannelDgraphUid: uids["general"]}}, both()...) {
		inChannel := carried(`{ q(func: eq(ch_uuid, "` + general.String() + `")) { messages: ch_posts @filter(NOT eq(post_uuid, "` + source.String() + `")) { ` + fmt.Sprintf(files, "post_attachments") + ` } } }`)
		if want := []string{inGeneral.String() + " " + sam.String() + "/general.png"}; !slices.Equal(inChannel, want) {
			t.Errorf("the forward in #general carries %v; want only sam's upload for it: %v", inChannel, want)
		}
	}
	if forward([]userAdapter.UserAndChannelFwdMessage{{GrpId: crew}}, both()...) {
		if got, want := inConversation(crew), []string{inCrew.String() + " " + sam.String() + "/crew.png"}; !slices.Equal(got, want) {
			t.Errorf("the forward in the group carries %v; want only sam's upload for it: %v", got, want)
		}
	}
}
