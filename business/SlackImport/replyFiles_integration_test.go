//go:build integration

package business

// A file a Slack thread reply brought is the reply's, as a reply's upload in
// the app is: the reply's comment lists it, the thread's message doesn't, and
// its search entry is the channel's, filed under the thread's message, so it
// is found only by those who may read the channel. The file stage looked a
// file's message up only among the import's messages, and a reply is a
// comment, so its file hung on nothing (FILE_PARENT_UNRESOLVED) and had no
// search entry.
//
// Run: PKGS=./business/SlackImport/ integ_rel.sh <wt> <log> 'TestAThreadReplysFileIsTheReplys|TestOnlyWhoMayReadAReplysPlaceFindsItsFile'

import (
	"archive/zip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	globalSearchBusiness "github.com/akashc777/OneCamp/business/GlobalSearch"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/models/dgexec"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/akashc777/OneCamp/tests/searchprobe"
)

func TestAThreadReplysFileIsTheReplys(t *testing.T) {
	w := newImportWorld(t)
	integration.SetupMinio(t, "slack-reply-files")
	// The search entries the import writes, as their bulk bodies.
	var mu sync.Mutex
	var entries []string
	search := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		entries = append(entries, string(body))
		mu.Unlock()
		fmt.Fprint(rw, `{"took":1,"errors":false,"items":[]}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	integration.UseOpenSearchClient(t, client)

	job := w.newJob(t, "")
	const rootTs, replyTs = "1792000000.000100", "1792000000.000200"
	root := `{"type":"message","user":"U1","text":"the plan","ts":"` + rootTs + `","thread_ts":"` + rootTs + `"}`
	reply := `{"type":"message","user":"U2","text":"notes attached","ts":"` + replyTs + `","thread_ts":"` + rootTs + `"}`
	w.run(t, w.chunk(t, job, importModels.ChunkChannelMessages, "C1"), "", root, reply)
	w.run(t, w.chunk(t, job, importModels.ChunkChannelThreads, "C1"), "", root, reply)
	post, perr := importModels.LookupIdMapping(w.ctx, job, importModels.EntityMessage, messageKey("C1", rootTs))
	comment, cerr := importModels.LookupIdMapping(w.ctx, job, importModels.EntityComment, messageKey("C1", replyTs))
	if perr != nil || cerr != nil || post == uuid.Nil || comment == uuid.Nil {
		t.Fatalf("the thread: post %s (%v), reply %s (%v)", post, perr, comment, cerr)
	}

	// The reply's file: its chunk as the reply's message schedules it, and its
	// bytes in the export.
	c := buildFileChunk(job, "C1", replyTs, SlackFile{ID: "F1", Name: "notes.txt", Size: 5,
		URLPrivateDownload: "https://files.slack.com/files-pri/T1-F1/notes.txt"})
	if err := importModels.CreateChunks(w.ctx, []*importModels.Chunk{c}); err != nil {
		t.Fatal(err)
	}
	_, zr := buildArchive(t, []zipTestEntry{{name: "F1/notes.txt", body: "notes"}})
	if err := processFileChunk(w.quiet, fileSource{uploads: map[string]*zip.File{"F1": zr.File[0]}}, c, w.admin, 0); err != nil {
		t.Fatalf("the file's chunk: %v", err)
	}
	file, err := importModels.LookupIdMapping(w.ctx, job, importModels.EntityFile, "F1")
	if err != nil || file == uuid.Nil {
		t.Fatalf("the file: %s (%v)", file, err)
	}

	type files []struct {
		ID string `json:"attachment_uuid"`
	}
	got, err := dgexec.Read[struct {
		Reply []struct {
			Files files `json:"comment_attachments"`
		} `json:"reply"`
		Post []struct {
			Files files `json:"post_attachments"`
		} `json:"post"`
	}](w.ctx, `query Files($reply: string, $post: string) {
		reply(func: eq(comment_uuid, $reply)) { uid comment_attachments { attachment_uuid } }
		post(func: eq(post_uuid, $post)) { uid post_attachments { attachment_uuid } }
	}`, map[string]string{"$reply": comment.String(), "$post": post.String()})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Reply) != 1 || len(got.Reply[0].Files) != 1 || got.Reply[0].Files[0].ID != file.String() {
		t.Errorf("the reply's files: %+v; want the file %s alone", got.Reply, file)
	}
	if len(got.Post) != 1 || len(got.Post[0].Files) != 0 {
		t.Errorf("the thread's message: %+v; want it, listing no file", got.Post)
	}
	w.logged(t, job, map[string]int{"FILE_PARENT_UNRESOLVED": 0})

	// The search entry is written on its own goroutine: let it finish.
	if busy := helpers.SettleBackground(10 * time.Second); len(busy) > 0 {
		t.Fatalf("still at work after 10 s: %v", busy)
	}
	var entry string
	mu.Lock()
	for _, e := range entries {
		if strings.Contains(e, file.String()) {
			entry = e
		}
	}
	mu.Unlock()
	for _, want := range []string{`"attachment_channel_id":"` + w.general.String() + `"`, `"attachment_post_id":"` + post.String() + `"`} {
		if !strings.Contains(entry, want) {
			t.Errorf("the file's search entry: %q; want it to hold %s", entry, want)
		}
	}
}

// Who finds a reply's file in search is whoever may read its place: a private
// channel's people, a DM's two. Against a real OpenSearch, through the search
// as its endpoint runs it. Before a reply's file had an entry, its place's own
// people never found it; an entry filed wider than its place (under #general
// for #launch's, or naming one more person than the DM's two) is found by
// someone outside.
func TestOnlyWhoMayReadAReplysPlaceFindsItsFile(t *testing.T) {
	w := newImportWorld(t)
	integration.SetupMinio(t, "slack-reply-search")
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}
	olga := w.person(t, "olga")
	cerr, launch := channelBusiness.CreateChannel(w.ctx, &channelAdapter.InputCreateChannel{ChannelName: "launch", ChannelPrivate: true}, w.admin)
	if cerr != nil {
		t.Fatal(cerr)
	}
	// The export's private #launch (C2), the admin's DM with ada (D1), and
	// olga (U3), in neither.
	job := w.newJob(t, "")
	w.exec(t, `INSERT INTO import_id_map (import_id, entity_type, source_id, onecamp_uuid)
		VALUES ($1, $2, 'C2', $3), ($1, $4, 'U3', $5)`,
		job, importModels.EntityChannel, launch, importModels.EntityUser, olga.UserPostgresInfo.Id)
	if _, err := resolveDMs(w.ctx, job, "", []SlackChannel{{ID: "D1", Members: []string{"U1", "U2"}}}, nil, w.admin); err != nil {
		t.Fatal(err)
	}

	// replyFile imports the admin's thread in conversation, whose reply brings
	// a file called name, and is the file the import made.
	replyFile := func(conversation, fileID, name string) uuid.UUID {
		t.Helper()
		const rootTs, replyTs = "1792000000.000100", "1792000000.000200"
		root := `{"type":"message","user":"U1","text":"the figures","ts":"` + rootTs + `","thread_ts":"` + rootTs + `"}`
		reply := `{"type":"message","user":"U1","text":"attached","ts":"` + replyTs + `","thread_ts":"` + rootTs + `"}`
		if conversation == "D1" {
			w.run(t, w.chunk(t, job, importModels.ChunkDMMessages, conversation), "", root, reply)
		} else {
			w.run(t, w.chunk(t, job, importModels.ChunkChannelMessages, conversation), "", root, reply)
			w.run(t, w.chunk(t, job, importModels.ChunkChannelThreads, conversation), "", root, reply)
		}
		c := buildFileChunk(job, conversation, replyTs, SlackFile{ID: fileID, Name: name, Size: 7,
			URLPrivateDownload: "https://files.slack.com/files-pri/T1-" + fileID + "/" + name})
		if err := importModels.CreateChunks(w.ctx, []*importModels.Chunk{c}); err != nil {
			t.Fatal(err)
		}
		_, zr := buildArchive(t, []zipTestEntry{{name: fileID + "/" + name, body: "figures"}})
		if err := processFileChunk(w.quiet, fileSource{uploads: map[string]*zip.File{fileID: zr.File[0]}}, c, w.admin, 0); err != nil {
			t.Fatalf("%s's file chunk: %v", conversation, err)
		}
		file, err := importModels.LookupIdMapping(w.ctx, job, importModels.EntityFile, fileID)
		if err != nil || file == uuid.Nil {
			t.Fatalf("%s's file: %s (%v)", conversation, file, err)
		}
		return file
	}
	inLaunch := replyFile("C2", "F1", "budget.txt")
	inDM := replyFile("D1", "F2", "contract.txt")
	// The search entries are written on their own goroutines: let them finish.
	if busy := helpers.SettleBackground(10 * time.Second); len(busy) > 0 {
		t.Fatalf("still at work after 10 s: %v", busy)
	}
	if _, err := opensearchInit.OpenSearchClient.Indices.Refresh(w.ctx, &opensearchapi.IndicesRefreshReq{Indices: []string{openSearchStruct.ATTACHMENT_INDEX}}); err != nil {
		t.Fatalf("refreshing the files index: %v", err)
	}

	// find searches text as p, in the channels given.
	find := func(p *userModels.UserInfo, text string, channels ...uuid.UUID) globalSearchBusiness.Result {
		t.Helper()
		reach := globalSearchBusiness.Reach{UserUUID: p.UserPostgresInfo.Id.String(), Email: p.UserPostgresInfo.EmailID}
		for _, c := range channels {
			reach.Channels = append(reach.Channels, c.String())
		}
		return searchprobe.SearchAs(t, w.ctx, reach, text, globalSearchBusiness.Page{})
	}
	shows := func(r globalSearchBusiness.Result, file uuid.UUID) bool {
		for _, h := range r.Page {
			if h.Attachment != nil && h.Attachment.Uuid == file.String() {
				return true
			}
		}
		return false
	}

	// #launch's: the admin, in it, finds it; ada, in #general alone, finds
	// nothing, on the page or in any count.
	if r := find(w.admin, "budget", w.general, launch); !shows(r, inLaunch) {
		t.Errorf("someone in #launch finds %d (%v) for budget, not the reply's file", r.Total, r.Totals)
	}
	if r := find(w.ada, "budget", w.general); r.Total != 0 || len(r.Totals) != 0 || shows(r, inLaunch) {
		t.Errorf("someone outside #launch finds %d (%v) for budget; want nothing", r.Total, r.Totals)
	}
	// The DM's: ada, the other one in it, finds it; olga finds nothing.
	if r := find(w.ada, "contract", w.general); !shows(r, inDM) {
		t.Errorf("the DM's other one finds %d (%v) for contract, not the reply's file", r.Total, r.Totals)
	}
	if r := find(olga, "contract", w.general); r.Total != 0 || len(r.Totals) != 0 || shows(r, inDM) {
		t.Errorf("someone outside the DM finds %d (%v) for contract; want nothing", r.Total, r.Totals)
	}
}
