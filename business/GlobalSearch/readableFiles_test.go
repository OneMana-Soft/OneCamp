package business

import (
	"context"
	"errors"
	"testing"

	fileAccessBusiness "github.com/akashc777/OneCamp/business/FileAccess"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
)

const (
	myProject       = "aaaaaaaa-0000-4000-8000-000000000001"
	teamProject     = "aaaaaaaa-0000-4000-8000-000000000002"
	adminProject    = "aaaaaaaa-0000-4000-8000-000000000003"
	unreadProject   = "aaaaaaaa-0000-4000-8000-000000000004"
	fileOfMyChannel = "chan-file"
)

func projectFile(project string) *openSearchStruct.GlobalSearchOpenSearchResp {
	return &openSearchStruct.GlobalSearchOpenSearchResp{Type: openSearchStruct.ATTACHMENT_TYPE,
		Attachment: &openSearchStruct.OpenSearchAttachment{Uuid: "f-" + project, AttachmentProjectUuid: project}}
}

// The index granted a project's files to everyone in its team, so someone in
// the team but not the project found its files' names. A project's file is
// now kept only for someone who may have the project's files: their own
// projects need no asking, any other is asked once a page, and one that can't
// be asked keeps nothing.
func TestSearchKeepsOnlyTheFilesOfProjectsThePersonIsIn(t *testing.T) {
	asked := map[string]int{}
	old := mayReadFiles
	mayReadFiles = func(_ context.Context, userUUID string, p fileAccessBusiness.Place) error {
		asked[p.Kind+" "+p.ID]++
		switch {
		case userUUID != "me":
			t.Fatalf("asked about %q", userUUID)
		case p.ID == adminProject:
			return nil
		case p.ID == unreadProject:
			return errors.New("the graph didn't answer")
		}
		return fileAccessBusiness.ErrRefused
	}
	t.Cleanup(func() { mayReadFiles = old })

	channelFile := &openSearchStruct.GlobalSearchOpenSearchResp{Type: openSearchStruct.ATTACHMENT_TYPE,
		Attachment: &openSearchStruct.OpenSearchAttachment{Uuid: fileOfMyChannel, AttachmentChannelUuid: "c1"}}
	hits := []*openSearchStruct.GlobalSearchOpenSearchResp{
		projectFile(myProject), projectFile(teamProject), projectFile(teamProject),
		projectFile(adminProject), projectFile(unreadProject), channelFile, task(liveTask),
	}
	mine := []*dgraphStruct.DgraphProject{{Uuid: myProject}}
	got := keepReadableProjectFileHits(context.Background(), "me", mine, hits)

	var kept []string
	for _, h := range got {
		switch {
		case h.Attachment != nil:
			kept = append(kept, h.Attachment.Uuid)
		case h.Task != nil:
			kept = append(kept, "task")
		}
	}
	want := []string{"f-" + myProject, "f-" + adminProject, fileOfMyChannel, "task"}
	if len(kept) != len(want) {
		t.Fatalf("kept %v, want %v", kept, want)
	}
	for i := range want {
		if kept[i] != want[i] {
			t.Fatalf("kept %v, want %v", kept, want)
		}
	}
	if asked["project "+myProject] != 0 || asked["project "+teamProject] != 1 {
		t.Fatalf("asked %v: the person's own project needs no asking, another is asked once a page", asked)
	}
}
