package business

// The work graph's tools for agents (DG8): related_items (what one thing is
// connected to) and how_connected (the shortest ways two things connect).
//
// NOTHING NEW DECIDES HERE. business/WorkGraph reads the graph over DG5's
// allow-list and names every node through business/EntityLink ReadMany, as
// the reader: the rule every chip, title and Connected panel in the app is
// decided by. A thing the reader can't see is "not found", the same as a
// thing that isn't there, and a path through anything hidden is dropped
// whole, so a private doc in the middle of a path is never named.
//
// WHO THE READERS ARE. A tool runs as userUUID: the person asking in the app,
// an agent's sponsor, or an MCP token's owner (as at most a member through a
// token: accessBusiness.SubjectFor). When an agent run acts for someone other
// than its sponsor (services/AI RunRequester), that person is a second reader
// and the answer is what both may see (WorkGraph NeighboursFor/PathsFor), the
// same intersection the semantic index makes with runPermissionFilter. A run
// whose asker can't be identified reads nothing.
//
// The start is also checked before a call runs, by its kind, through
// PrincipalCanReach (business/MCPServer bridge.go's KindArg rows), for an MCP
// call and for an agent asked by someone else; this file still decides every
// node for every reader, so a call that skipped that gate would see no more.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	workGraph "github.com/akashc777/OneCamp/business/WorkGraph"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// The words a tool answers with when the work graph can't help. Hidden and
// missing are one sentence, so an answer can't be used to probe what exists.
const (
	graphNotFound = "That item isn't there, or you can't open it."
	graphBadAsk   = "That isn't something I can look up: name an item by its kind (doc, task, project, person, board, channel, message or team) and its uuid."
)

// graph reads, as seams for tests.
var (
	graphNeighbours = workGraph.NeighboursFor
	graphPaths      = workGraph.PathsFor
)

// graphReaders is who a graph tool's answer must be readable by: the person
// the tool runs as and, when an agent run acts for someone else, that person
// too. An error is a refusal: the asker couldn't be identified or resolved.
func graphReaders(ctx context.Context, userUUID string) ([]accessBusiness.Subject, error) {
	u, err := personByUUID(ctx, userUUID)
	if err != nil || u == nil {
		return nil, fmt.Errorf("could not resolve who is asking")
	}
	return askReaders(ctx, u)
}

// askReaders is who a work-graph answer must be readable by: the person asking
// and, when an agent run acts for someone other than its sponsor, that person
// too. An error means the asker couldn't be resolved: nothing is added.
func askReaders(ctx context.Context, userInfo *userModels.UserInfo) ([]accessBusiness.Subject, error) {
	who := accessBusiness.SubjectFor(ctx, userInfo)
	who.AgentUUID = strings.TrimSpace(ai.AgentBudgetID(ctx))
	readers := []accessBusiness.Subject{who}
	requester, _, forOther := ai.RunRequester(ctx)
	if !forOther {
		return readers, nil
	}
	if strings.TrimSpace(requester) == "" {
		return nil, errAskerUnknown
	}
	asker, err := askerInfo(ctx, strings.TrimSpace(requester))
	if err != nil {
		return nil, err
	}
	them := accessBusiness.SubjectFor(ctx, asker)
	them.AgentUUID = who.AgentUUID
	return append(readers, them), nil
}

// executeRelatedItems is related_items: what a thing is connected to.
func executeRelatedItems(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	p := action.Params
	depth := 1
	if d := strings.TrimSpace(p["depth"]); d != "" {
		n, err := strconv.Atoi(d)
		if err != nil || (n != 1 && n != 2) {
			return graphBadAsk, nil, nil
		}
		depth = n
	}
	kinds, err := workGraph.ParseKinds(p["kinds"])
	if err != nil {
		return graphBadAsk, nil, nil
	}
	readers, err := graphReaders(ctx, userUUID)
	if err != nil {
		return "", nil, err
	}
	root := workGraph.Ref{Kind: strings.ToLower(strings.TrimSpace(p["kind"])), ID: strings.TrimSpace(p["uuid"])}
	got, err := graphNeighbours(ctx, readers, root, depth, kinds)
	switch {
	case errors.Is(err, workGraph.ErrBadRequest):
		return graphBadAsk, nil, nil
	case errors.Is(err, workGraph.ErrNotFound):
		return graphNotFound, nil, nil
	case err != nil:
		helpers.LogErrorWithContext(ctx, "business/AI related_items: %+v", err)
		return "", nil, fmt.Errorf("couldn't read what this is connected to just now")
	}
	return describeConnected(got), map[string]string{"kind": got.Root.Kind, "uuid": got.Root.ID, "items": strconv.Itoa(countItems(got))}, nil
}

// executeHowConnected is how_connected: the shortest ways two things connect.
func executeHowConnected(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	p := action.Params
	readers, err := graphReaders(ctx, userUUID)
	if err != nil {
		return "", nil, err
	}
	from := workGraph.Ref{Kind: strings.ToLower(strings.TrimSpace(p["from_kind"])), ID: strings.TrimSpace(p["from_uuid"])}
	to := workGraph.Ref{Kind: strings.ToLower(strings.TrimSpace(p["to_kind"])), ID: strings.TrimSpace(p["to_uuid"])}
	got, err := graphPaths(ctx, readers, from, to)
	switch {
	case errors.Is(err, workGraph.ErrBadRequest):
		return graphBadAsk, nil, nil
	case errors.Is(err, workGraph.ErrNotFound):
		return "One of those items isn't there, or you can't open it.", nil, nil
	case err != nil:
		helpers.LogErrorWithContext(ctx, "business/AI how_connected: %+v", err)
		return "", nil, fmt.Errorf("couldn't read how these connect just now")
	}
	return describePaths(got), map[string]string{"paths": strconv.Itoa(len(got.Paths))}, nil
}

// relWords is how a relationship reads in a sentence, from the earlier
// thing's side. An unknown one reads as its name.
var relWords = map[string]string{
	"linked_from":  "linked from",
	"links_to":     "links to",
	"links_board":  "links the board",
	"mentioned_by": "mentioned by",
	"parent":       "inside",
	"subpage":      "has the page",
	"shared_in":    "shared in",
	"created_by":   "created by",
	"owned_by":     "owned by",
	"comment":      "has the comment",
	"in_project":   "in the project",
	"assigned_to":  "assigned to",
	"blocked_by":   "blocked by",
	"blocks":       "blocks",
	"subtask_of":   "a subtask of",
	"subtask":      "has the subtask",
	"task":         "has the task",
	"in_team":      "in the team",
	"memory":       "remembered as",
	"shares":       "shares",
	"posted_by":    "posted by",
	"in_channel":   "in the channel",
	"message":      "has the message",
	"created":      "created",
	"owns":         "owns",
	"assigned":     "assigned",
	"posted":       "posted",
	"project":      "has the project",
	"on":           "on",
	"about":        "about",
}

// relPhrase is how one relationship of the work graph reads in a sentence.
func relPhrase(rel string) string {
	if w, ok := relWords[rel]; ok {
		return w
	}
	return strings.ReplaceAll(rel, "_", " ")
}

// named is a thing as an answer cites it: its kind, its title in quotes and
// its uuid, which the tools that read it take. A title is someone's words,
// so it is cut short and its quotes are softened.
func named(kind, title, id string) string {
	t := strings.TrimSpace(strings.ReplaceAll(title, "\"", "'"))
	if t == "" {
		t = "untitled"
	}
	return fmt.Sprintf("%s \"%s\" (%s)", kind, helpers.TruncateRunes(t, 80), id)
}

// pathWords is a path as a sentence from the thing it starts at.
func pathWords(hops []workGraph.Hop) string {
	parts := make([]string, 0, len(hops))
	for _, h := range hops {
		parts = append(parts, relPhrase(h.Rel)+" "+named(h.Kind, h.Title, h.ID))
	}
	return strings.Join(parts, ", then ")
}

func describeConnected(c *workGraph.Connected) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Connected to %s:\n", named(c.Root.Kind, c.Root.Title, c.Root.ID))
	if countItems(c) == 0 {
		b.WriteString("Nothing you can open is connected to it.\n")
		return b.String()
	}
	for _, g := range c.Groups {
		fmt.Fprintf(&b, "\n%s:\n", g.Kind)
		for _, it := range g.Items {
			fmt.Fprintf(&b, "- %s: %s\n", named(it.Kind, it.Title, it.ID), pathWords(it.Path))
		}
		if g.More {
			fmt.Fprintf(&b, "- and more of these than are listed\n")
		}
	}
	return b.String()
}

func describePaths(p *workGraph.Paths) string {
	var b strings.Builder
	from, to := named(p.From.Kind, p.From.Title, p.From.ID), named(p.To.Kind, p.To.Title, p.To.ID)
	if len(p.Paths) == 0 {
		fmt.Fprintf(&b, "No connection you can see links %s to %s within four steps.\n", from, to)
		return b.String()
	}
	fmt.Fprintf(&b, "How %s connects to %s:\n", from, to)
	for i, path := range p.Paths {
		fmt.Fprintf(&b, "%d. %s %s\n", i+1, from, pathWords(path.Hops))
	}
	return b.String()
}

func countItems(c *workGraph.Connected) int {
	n := 0
	for _, g := range c.Groups {
		n += len(g.Items)
	}
	return n
}
