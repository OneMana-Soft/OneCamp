package asana

import (
	"context"
	"fmt"
	"net/url"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

// A task's files (I1): Asana's task doesn't list them, so the comments stage
// asks for them here, with the task's comments (importProvider.TaskFileLister),
// and each is fetched as a file of the task (FetchAttachment refreshes its
// short-lived download address).

// ListTaskFiles is the files kept in Asana on a task.
func (p *Provider) ListTaskFiles(ctx context.Context, j *importModels.Job, _ importProvider.JobOptions, taskSourceId string) ([]importProvider.SourceAttachment, error) {
	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return nil, err
	}
	return p.listTaskFiles(ctx, tok, taskSourceId)
}

func (p *Provider) listTaskFiles(ctx context.Context, tok, taskGID string) ([]importProvider.SourceAttachment, error) {
	type file struct {
		GID         string `json:"gid"`
		Name        string `json:"name"`
		Size        int64  `json:"size"`
		Host        string `json:"host"`
		DownloadURL string `json:"download_url"`
	}
	type filesResp struct {
		Data     []file         `json:"data"`
		NextPage *asanaNextPage `json:"next_page"`
	}
	var out []importProvider.SourceAttachment
	path := fmt.Sprintf("/attachments?parent=%s&limit=100&opt_fields=gid,name,size,host,download_url", url.QueryEscape(taskGID))
	for page := 0; path != "" && page < maxPages; page++ {
		var resp filesResp
		if err := p.getJSON(ctx, tok, path, &resp); err != nil {
			return out, err
		}
		for _, f := range resp.Data {
			// A link to a file in another service (Drive, Dropbox) has nothing to download.
			if f.Host != "asana" || f.DownloadURL == "" {
				continue
			}
			out = append(out, importProvider.SourceAttachment{
				SourceID: f.GID,
				Name:     f.Name,
				URL:      f.DownloadURL,
				Size:     f.Size,
				Parent:   importProvider.SourceRef{Kind: "task", SourceID: taskGID},
			})
		}
		path = nextPagePath(resp.NextPage)
	}
	return out, nil
}
