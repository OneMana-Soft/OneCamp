package business

// find_people's "who knows about …" (DG9, AI edition): the shared Who knows
// (business/WhoKnows), asked as the person the tool runs for, with the
// assistant's own meaning-based search (kNN) added to the keyword seeds. The
// shared read decides every item as that person, counts nothing they can't
// open and never names anyone who left themselves out; this file only adds
// seeds and words the answer for the model. The search page's strip uses
// keyword seeds alone in both editions: an embedding per keystroke would
// spend the AI budget on a sidebar.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	whoKnowsBusiness "github.com/akashc777/OneCamp/business/WhoKnows"
	whoKnowsDomain "github.com/akashc777/OneCamp/domain/WhoKnows"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// whoKnowsKNN is how many meaning-based hits find_people adds as seeds.
const whoKnowsKNN = 50

// askWhoKnows is whoKnowsBusiness.Ask (a seam for tests).
var askWhoKnows = whoKnowsBusiness.Ask

// knnSeeds is the meaning-based search for who, as items: a doc, a task or a
// channel post, and a comment as the thing it's on. Through searchSimilar,
// the doc check every workspace search goes through (docHits.go).
func knnSeeds(who *userModels.UserInfo) whoKnowsBusiness.SeedSource {
	return func(ctx context.Context, asker whoKnowsBusiness.Asker, text string, _ int) ([]whoKnowsDomain.Seed, error) {
		hits, err := searchSimilar(ctx, who, text, asker.Channels, asker.Projects, nil, whoKnowsKNN)
		if err != nil {
			return nil, err
		}
		var out []whoKnowsDomain.Seed
		for _, h := range hits {
			if s, ok := seedOfHit(h); ok {
				out = append(out, s)
			}
		}
		return out, nil
	}
}

// seedOfHit is the item a kNN hit stands for: none for chats and anything
// that isn't a doc, a task or a channel post.
func seedOfHit(h ai.SimilarResult) (whoKnowsDomain.Seed, bool) {
	switch h.ContentType {
	case "doc":
		return whoKnowsDomain.Seed{Kind: whoKnowsDomain.KindDoc, ID: h.ContentUUID}, h.ContentUUID != ""
	case "task":
		return whoKnowsDomain.Seed{Kind: whoKnowsDomain.KindTask, ID: h.ContentUUID}, h.ContentUUID != ""
	case "post":
		return whoKnowsDomain.Seed{Kind: whoKnowsDomain.KindMessage, ID: h.ContentUUID}, h.ContentUUID != ""
	case "comment":
		switch {
		case h.PostUUID != "":
			return whoKnowsDomain.Seed{Kind: whoKnowsDomain.KindMessage, ID: h.PostUUID}, true
		case h.TaskUUID != "":
			return whoKnowsDomain.Seed{Kind: whoKnowsDomain.KindTask, ID: h.TaskUUID}, true
		case h.DocUUID != "":
			return whoKnowsDomain.Seed{Kind: whoKnowsDomain.KindDoc, ID: h.DocUUID}, true
		}
	}
	return whoKnowsDomain.Seed{}, false
}

// askerFor is who a tool call asks as: accessBusiness.SubjectFor (a member at
// most through a token), with the reach the index may grant them.
func askerFor(ctx context.Context, u *userModels.UserInfo) whoKnowsBusiness.Asker {
	a := whoKnowsBusiness.Asker{Subject: accessBusiness.SubjectFor(ctx, u), Email: u.UserPostgresInfo.EmailID}
	for _, c := range u.UserDgraphInfo.Channels {
		if c != nil {
			a.Channels = append(a.Channels, c.Uuid)
		}
	}
	for _, p := range u.UserDgraphInfo.Projects {
		if p != nil {
			a.Projects = append(a.Projects, p.Uuid)
		}
	}
	for _, t := range u.UserDgraphInfo.Teams {
		if t != nil {
			a.Teams = append(a.Teams, t.Uuid)
		}
	}
	return a
}

// whoKnowsText is find_people's answer to "who knows about topic", for the
// model: each person with what they did and the items that show it, by title
// and id, so it can cite them. Never a count of what was left out.
func whoKnowsText(ctx context.Context, caller *userModels.UserInfo, topic string) (string, error) {
	got, err := askWhoKnows(ctx, askerFor(ctx, caller), topic, 0, knnSeeds(caller))
	if err != nil {
		if errors.Is(err, whoKnowsBusiness.ErrBadRequest) {
			return "", fmt.Errorf("topic must be 2 to 200 characters")
		}
		return "", err
	}
	if len(got.People) == 0 {
		return fmt.Sprintf("No one has written, been assigned or commented on anything about %q that the user can open.", topic), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "People who know about %q, from work the user can open (most first):\n", topic)
	for _, p := range got.People {
		var did []string
		if p.Wrote > 0 {
			did = append(did, fmt.Sprintf("wrote %d docs", p.Wrote))
		}
		if p.Created > 0 {
			did = append(did, fmt.Sprintf("created %d tasks", p.Created))
		}
		if p.Posted > 0 {
			did = append(did, fmt.Sprintf("posted %d messages", p.Posted))
		}
		if p.Assigned > 0 {
			did = append(did, fmt.Sprintf("assigned %d tasks", p.Assigned))
		}
		if p.Commented > 0 {
			did = append(did, fmt.Sprintf("%d comments", p.Commented))
		}
		fmt.Fprintf(&b, "- %s [%s]: %s\n", helpers.OneLine(p.Name, 80), p.ID, strings.Join(did, ", "))
		for _, e := range p.Evidence {
			fmt.Fprintf(&b, "  - %s %s %q [%s]\n", e.Role, e.Kind, helpers.OneLine(e.Title, 100), e.ID)
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
