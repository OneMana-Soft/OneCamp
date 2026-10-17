package models

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
)

// A rename carries the name people see into every index that names someone
// by it, as who wrote a post, a chat, a comment, a file or a doc and whose
// task it is. The users index holds the person's own display name and full
// name instead, written by UpdateUserInOpenSearch in the same call, so only
// the profile goes there: the name people see copied over both replaced the
// full name, and a renamed member could no longer be found by it.
func TestARenameLeavesTheNamesInTheUsersIndexAlone(t *testing.T) {
	var mu sync.Mutex
	scripts := map[string]string{} // index -> the update script sent to it
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Script struct {
				Source string `json:"source"`
			} `json:"script"`
		}
		if index, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/_update_by_query"); ok && json.NewDecoder(r.Body).Decode(&body) == nil {
			mu.Lock()
			scripts[index] = body.Script.Source
			mu.Unlock()
		}
		fmt.Fprint(w, `{"took":1,"total":0,"updated":0,"failures":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	defer func() { opensearchInit.OpenSearchClient = nil }()

	if err := PropagateUserInfoChangeInOpenSearch(context.Background(), "u-1", "Sam", "photo-1"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	users := scripts[openSearchStruct.USER_INDEX]
	if !strings.Contains(users, "ctx._source.user_profile_object_key = params.newProfile") {
		t.Fatalf("the users index was not sent the new profile: %q", users)
	}
	for _, field := range []string{"user_name", "user_full_name", "params.newName"} {
		if strings.Contains(users, field) {
			t.Errorf("the users index script touches %s: %q", field, users)
		}
	}
	for index, author := range map[string]string{
		openSearchStruct.POST_INDEX:       "post_by_user_full_name = params.newName",
		openSearchStruct.CHAT_INDEX:       "chat_by_user_full_name = params.newName",
		openSearchStruct.COMMENT_INDEX:    "comment_by_user_full_name = params.newName",
		openSearchStruct.ATTACHMENT_INDEX: "attachment_by_user_full_name = params.newName",
		openSearchStruct.DOC_INDEX:        "doc_created_by_user_full_name = params.newName",
		openSearchStruct.TASK_INDEX:       "task_assignee_user_full_name = params.newName",
	} {
		if !strings.Contains(scripts[index], author) {
			t.Errorf("%s does not take the name people see (%s): %q", index, author, scripts[index])
		}
	}
}

// A rename rewrites the entries another write was holding as it ran. With
// conflicts "proceed" an entry being written at the same moment is skipped:
// not written and not counted, so it kept the old name until its next write,
// which for an old post never comes. Here every index's first run meets one
// such entry, and the name must be written by running again.
func TestARenameRewritesEntriesAnotherWriteWasHolding(t *testing.T) {
	var mu sync.Mutex
	runs := map[string]int{} // index -> update-by-query runs it was sent
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/_update_by_query")
		if !ok {
			fmt.Fprint(w, `{}`)
			return
		}
		mu.Lock()
		runs[index]++
		first := runs[index] == 1
		mu.Unlock()
		if first {
			fmt.Fprint(w, `{"took":1,"total":3,"updated":2,"version_conflicts":1,"failures":[]}`)
			return
		}
		fmt.Fprint(w, `{"took":1,"total":1,"updated":1,"version_conflicts":0,"failures":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	defer func() { opensearchInit.OpenSearchClient = nil }()

	if err := PropagateUserInfoChangeInOpenSearch(context.Background(), "u-1", "Sam", "photo-1"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, index := range []string{openSearchStruct.POST_INDEX, openSearchStruct.CHAT_INDEX, openSearchStruct.COMMENT_INDEX,
		openSearchStruct.ATTACHMENT_INDEX, openSearchStruct.DOC_INDEX, openSearchStruct.TASK_INDEX, openSearchStruct.USER_INDEX} {
		if runs[index] != 2 {
			t.Errorf("%s was run %d times; want a second run for the entry another write held", index, runs[index])
		}
	}
}
