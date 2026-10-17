package business

// list_docs: the docs the person can open, newest edit first, for an agent or
// an MCP client to find a doc before reading it or adding to it. It is the
// public API's GET /v1/docs list (business/ContentIO ListDocs). When someone
// other than the agent's sponsor asked for the run, only the docs that person
// can open too are listed.

import (
	"context"
	"fmt"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// listDocsMax is how many docs one call lists.
const listDocsMax = 50

// Seams for tests: the list, and the person a run is for.
var (
	listDocsFor   = contentIOBusiness.ListDocs
	docListReader = func(ctx context.Context, userUUID string) ([]docReader, bool, error) {
		info, err := getUserInfoForExecutor(ctx, userUUID)
		if err != nil {
			return nil, false, err
		}
		readers, known := docReaders(ctx, info)
		return readers, known, nil
	}
)

func executeListDocs(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	scope := strings.TrimSpace(action.Params["scope"])
	switch scope {
	case "":
		scope = docDomain.ScopeWorkspace
	case docDomain.ScopeWorkspace, docDomain.ScopeMine, docDomain.ScopeShared:
	default:
		return "", nil, fmt.Errorf("scope is workspace, mine or shared")
	}
	readers, known, err := docListReader(ctx, userUUID)
	if err != nil {
		return "", nil, err
	}
	if !known {
		return "", nil, errAskerUnknown
	}
	who := accessBusiness.Subject{UserUID: readers[0].uid, UserUUID: readers[0].uuid}
	docs, more, err := listDocsFor(ctx, who, scope, listDocsMax, 0)
	if err != nil {
		return "", nil, fmt.Errorf("the docs could not be listed just now")
	}
	if len(readers) > 1 && len(docs) > 0 {
		asker := accessBusiness.Subject{UserUID: readers[1].uid, UserUUID: readers[1].uuid}
		res := make([]accessBusiness.Resource, len(docs))
		for i, d := range docs {
			res[i] = accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: d.UUID}
		}
		kept, err := accessBusiness.Rules.Filter(ctx, asker, res, accessBusiness.LevelView)
		if err != nil {
			return "", nil, fmt.Errorf("could not read which docs the person who asked can open")
		}
		open := make(map[string]bool, len(kept))
		for _, k := range kept {
			open[k.UUID] = true
		}
		mine := docs[:0]
		for _, d := range docs {
			if open[d.UUID] {
				mine = append(mine, d)
			}
		}
		docs = mine
	}
	if len(docs) == 0 {
		return "No docs to list there.", nil, nil
	}
	var b strings.Builder
	b.WriteString("Docs, newest edit first:\n")
	for _, d := range docs {
		title := strings.TrimSpace(d.Title)
		if title == "" {
			title = "Untitled"
		}
		private := ""
		if d.Private {
			private = ", private"
		}
		b.WriteString(fmt.Sprintf("- %s (doc_uuid: %s%s)\n", title, d.UUID, private))
	}
	if more {
		b.WriteString(fmt.Sprintf("These are the newest %d; search_workspace finds a doc by what it says.", listDocsMax))
	}
	return strings.TrimSpace(b.String()), nil, nil
}
