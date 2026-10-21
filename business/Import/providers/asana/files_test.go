package asana

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// I1: Asana imported no comments (a task carries no count) and no files (a
// task doesn't list them). A task says it can't count its comments, so the
// comments stage asks; that stage lists the task's files too.

func TestATaskSaysItCantCountItsComments(t *testing.T) {
	if got := buildSourceTask(&asanaTask{GID: "1", Name: "T"}, "p", nil, "").CommentCount; got != importProvider.CommentsUnknown {
		t.Fatalf("comment count %d, want CommentsUnknown", got)
	}
}

func TestATasksFilesAreListedPageByPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/1.0/attachments" || r.URL.Query().Get("parent") != "1" {
			t.Errorf("asked %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if r.URL.Query().Get("offset") == "" {
			fmt.Fprint(w, `{"data":[{"gid":"a1","name":"spec.pdf","size":10,"host":"asana","download_url":"https://s3/a1"},{"gid":"a2","name":"link","host":"external","view_url":"https://drive/x"}],
			  "next_page":{"offset":"x","path":"/api/1.0/attachments?parent=1&limit=100&offset=x"}}`)
			return
		}
		fmt.Fprint(w, `{"data":[{"gid":"a3","name":"plan.png","size":20,"host":"asana","download_url":"https://s3/a3"}],"next_page":null}`)
	}))
	defer srv.Close()
	defer func(old string) { apiBase = old }(apiBase)
	apiBase = srv.URL + "/api/1.0"

	files, err := New().listTaskFiles(context.Background(), "tok", "1")
	if err != nil {
		t.Fatal(err)
	}
	// Files kept in Asana come; a link to another service isn't a file to fetch.
	if len(files) != 2 || files[0].SourceID != "a1" || files[1].SourceID != "a3" || files[0].Parent.Kind != "task" || files[0].Parent.SourceID != "1" {
		t.Fatalf("files: %+v", files)
	}
}
