package business

// Search hits on a project's file are kept only if the person may have the
// project's files.
//
// The index query grants a project's files to the projects of everyone's
// teams as well as their own (GetUnifiedGlobalSearchPage folds the team's
// projects in), but a project's files are its members' and admins'
// (fileAccessBusiness.MayRead), as the route that serves them says: someone
// in a team found the names of the files of its projects they weren't in. A
// hit on a file of one of the person's own projects is kept as it is; any
// other is asked of the project, once a project a page. A project that can't
// be asked keeps none of its files: refusing costs a search result, keeping
// a wrong one costs a private file's name.

import (
	"context"
	"strings"

	fileAccessBusiness "github.com/akashc777/OneCamp/business/FileAccess"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	postgresStruct "github.com/akashc777/OneCamp/models/postgres"
)

// mayReadFiles is fileAccessBusiness.MayRead. A seam for tests.
var mayReadFiles = fileAccessBusiness.MayRead

// keepReadableProjectFileHits returns hits without those on a file of a
// project the person (by uuid) may not have the files of. mine are the
// person's own projects, which need no asking. Hits on anything else are kept
// as they are.
func keepReadableProjectFileHits(ctx context.Context, userUUID string, mine []*dgraphStruct.DgraphProject, hits []*openSearchStruct.GlobalSearchOpenSearchResp) []*openSearchStruct.GlobalSearchOpenSearchResp {
	readable := map[string]bool{}
	for _, p := range mine {
		if p != nil && p.Uuid != "" {
			readable[strings.ToLower(p.Uuid)] = true
		}
	}
	kept := make([]*openSearchStruct.GlobalSearchOpenSearchResp, 0, len(hits))
	dropped := 0
	for _, h := range hits {
		if h == nil || h.Attachment == nil || h.Attachment.AttachmentProjectUuid == "" {
			kept = append(kept, h)
			continue
		}
		project := strings.ToLower(h.Attachment.AttachmentProjectUuid)
		yes, asked := readable[project]
		if !asked {
			err := mayReadFiles(ctx, userUUID, fileAccessBusiness.Place{Kind: postgresStruct.ATTACHMENT_SRC_PROJECT, ID: project})
			if err != nil && !fileAccessBusiness.IsRefusal(err) {
				helpers.LogErrorWithContext(ctx, "search: could not check project %s's files, leaving them out: %v", project, err)
			}
			yes = err == nil
			readable[project] = yes
		}
		if !yes {
			dropped++
			continue
		}
		kept = append(kept, h)
	}
	if dropped > 0 {
		helpers.LogInfoWithContext(ctx, "search: left out %d results on files of projects the person isn't in", dropped)
	}
	return kept
}
