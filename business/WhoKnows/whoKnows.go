// Package whoKnowsBusiness is "Who knows about X" (DG9): the people whose
// work on X the asker can read, ranked, each with the items that show it.
//
// HOW IT ANSWERS.
//
//  1. Seeds: the items the words match, up to MaxSeeds, best first. The
//     keyword search (OpenSearch, both editions) always; any source the
//     caller adds (the assistant's kNN search, AI edition) after it.
//  2. Decide: every seed through business/EntityLink ReadMany, as the asker:
//     the one rule every chip, title and work-graph node is decided by (the
//     Access Resolver for docs, the task view's rule for tasks, the channel's
//     decision for a post). Only what the asker may open goes on. The index's
//     copies of who may read what can be stale, so nothing it says is
//     trusted.
//  3. Count: one DQL read (domain/WhoKnows) of who wrote, is assigned to and
//     commented on those items, with @groupby into value variables, live
//     people only, never a bot or an external account.
//  4. Rank: anyone who asked to be left out is removed (Postgres, before the
//     ranking, so they are never named and never counted towards a place),
//     and so is the asker (their own work is no answer to their question),
//     the demo's visitor sees only the demo's people, and the rest are ranked
//     by score = 3·wrote + 2·assigned + 1·comments.
//
// WHAT IT NEVER DOES. Count or show an item the asker can't open: a person
// whose only work on X is hidden from the asker isn't named, and evidence is
// only ever a decided item (the path person ← comment ← item is dropped whole
// when the item is hidden). Say how much was left out. Take a graph uid from
// anyone. Grant anything through a token: who is accessBusiness.SubjectFor's,
// a member at most, and nothing here reads Admin.
package whoKnowsBusiness

import (
	"context"
	"errors"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	entityLinkBusiness "github.com/akashc777/OneCamp/business/EntityLink"
	domain "github.com/akashc777/OneCamp/domain/WhoKnows"
	"github.com/akashc777/OneCamp/helpers"
	optOutModels "github.com/akashc777/OneCamp/models/postgres/WhoKnows"
)

// ErrBadRequest is words or a limit Who knows doesn't take.
var ErrBadRequest = errors.New("who knows: bad request")

// ErrNoOne is an asker who isn't a signed-in person.
var ErrNoOne = errors.New("who knows: no one asked")

const (
	// MaxSeeds is how many items one answer looks at, from every source.
	MaxSeeds = 200
	// DefaultLimit and MaxLimit are how many people an answer names.
	DefaultLimit = 8
	MaxLimit     = 20
	// EvidencePerPerson is how many items a person's line shows.
	EvidencePerPerson = 3
	// MinQuery and MaxQuery bound the words, in characters.
	MinQuery = 2
	MaxQuery = 200
)

// Weights of each role in a person's score.
const (
	weightWrote     = 3
	weightAssigned  = 2
	weightCommented = 1
)

// Asker is who asks: the person the Access Resolver decides for, their
// address (for the demo's list), and the reach the index may grant them
// (their channels, projects and teams, as the unified search reads them).
type Asker struct {
	Subject  accessBusiness.Subject
	Email    string
	Channels []string
	Projects []string
	Teams    []string
}

// Evidence is one item that shows a person knows: as the asker may see it.
type Evidence struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Title string `json:"title"`
	Role  string `json:"role"`
	// Data is what the item's chip shows the asker (a message's channel, to
	// open it there): the decided entity's own, never read again.
	Data map[string]any `json:"data,omitempty"`
}

// Person is one person named, with their counts over the items the asker can
// read and up to EvidencePerPerson of those items.
type Person struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	AvatarKey string     `json:"avatar_key,omitempty"`
	Score     int        `json:"score"`
	Wrote     int        `json:"wrote"`   // docs they wrote
	Created   int        `json:"created"` // tasks they created
	Posted    int        `json:"posted"`  // channel messages they posted
	Assigned  int        `json:"assigned"`
	Commented int        `json:"commented"`
	Evidence  []Evidence `json:"evidence"`
}

// Answer is who knows about the words.
type Answer struct {
	Query string `json:"query"`
	// Considered is how many items the asker can read were looked at. Never
	// how many were left out.
	Considered int      `json:"considered"`
	People     []Person `json:"people"`
	// YouHidden is whether the asker has asked to be left out.
	YouHidden bool `json:"you_hidden"`
}

// SeedSource finds items matching text for asker, up to n, best first: a
// search Ask adds after the keyword search (the AI edition's kNN, which only
// the assistant asks for: an embedding per keystroke on the search page would
// spend the AI budget on a sidebar). One that fails is logged and skipped.
type SeedSource func(ctx context.Context, asker Asker, text string, n int) ([]domain.Seed, error)

// Seams for tests.
var (
	keywordSeeds = func(ctx context.Context, a Asker, text string, n int) ([]domain.Seed, error) {
		return domain.KeywordSeeds(ctx, domain.Reach{UserUUID: a.Subject.UserUUID, Channels: a.Channels, Projects: a.Projects, Teams: a.Teams}, text, n)
	}
	readMany = entityLinkBusiness.ReadMany
	count    = domain.Count
	optedOut = optOutModels.OptedOut
)

// CleanQuery is the words as Who knows takes them: trimmed, MinQuery to
// MaxQuery characters; ErrBadRequest otherwise.
func CleanQuery(q string) (string, error) {
	q = strings.Join(strings.Fields(q), " ")
	if n := utf8.RuneCountInString(q); n < MinQuery || n > MaxQuery {
		return "", ErrBadRequest
	}
	return q, nil
}

// Ask is who knows about text, as asker may see it: at most limit people
// (DefaultLimit when 0), from the keyword seeds and then more's.
func Ask(ctx context.Context, asker Asker, text string, limit int, more ...SeedSource) (*Answer, error) {
	q, err := CleanQuery(text)
	if err != nil {
		return nil, err
	}
	if limit == 0 {
		limit = DefaultLimit
	}
	if limit < 1 || limit > MaxLimit {
		return nil, ErrBadRequest
	}
	if asker.Subject.UserUUID == "" {
		return nil, ErrNoOne
	}
	seeds, err := keywordSeeds(ctx, asker, q, MaxSeeds)
	if err != nil {
		return nil, err
	}
	for i, find := range more {
		if len(seeds) >= MaxSeeds || find == nil {
			break
		}
		got, err := find(ctx, asker, q, MaxSeeds)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "who knows: seed source %d failed, answering without it: %v", i+1, err)
			continue
		}
		seeds = append(seeds, got...)
	}
	return Rank(ctx, asker, q, seeds, limit)
}

// Rank is who knows about the items seeds name, as asker may see them: the
// steps after the search (decide, count, rank). Seeds past MaxSeeds are left
// out, the first ones kept.
func Rank(ctx context.Context, asker Asker, query string, seeds []domain.Seed, limit int) (*Answer, error) {
	if asker.Subject.UserUUID == "" {
		return nil, ErrNoOne
	}
	if limit < 1 || limit > MaxLimit {
		limit = DefaultLimit
	}
	seeds = uniqueSeeds(seeds, MaxSeeds)

	// 2. Decide: only what the asker may open goes on.
	readable, err := decide(ctx, asker.Subject, seeds)
	if err != nil {
		return nil, err
	}
	items := domain.Items{}
	for _, s := range seeds {
		if _, ok := readable[s]; !ok {
			continue
		}
		switch s.Kind {
		case domain.KindDoc:
			items.Docs = append(items.Docs, s.ID)
		case domain.KindTask:
			items.Tasks = append(items.Tasks, s.ID)
		case domain.KindMessage:
			items.Messages = append(items.Messages, s.ID)
		}
	}
	out := &Answer{Query: query, Considered: len(readable), People: []Person{}}

	// 3. Count.
	counted := &domain.Counted{}
	if !items.Empty() {
		if counted, err = count(ctx, items); err != nil {
			return nil, err
		}
	}

	// 4. Rank: never anyone who asked to be left out. The asker's own
	// setting is read in the same query.
	ids := []string{asker.Subject.UserUUID}
	for _, p := range counted.People {
		ids = append(ids, p.UUID)
	}
	left, err := optedOut(ctx, ids)
	if err != nil {
		return nil, err
	}
	out.YouHidden = left[strings.ToLower(asker.Subject.UserUUID)]
	var ranked []Person
	for _, p := range counted.People {
		// Never anyone who left themselves out, never the asker themself
		// (asking yourself is noise; DGRAPH-LEAD-2, 19 Oct), and to the demo's
		// visitor only the demo's own people.
		if left[p.UUID] || p.UUID == strings.ToLower(asker.Subject.UserUUID) || !helpers.DemoListsPerson(asker.Email, p.Email, false) {
			continue
		}
		ranked = append(ranked, Person{ID: p.UUID, Wrote: p.Wrote, Created: p.Created, Posted: p.Posted, Assigned: p.Assigned, Commented: p.Commented,
			Score: weightWrote*(p.Wrote+p.Created+p.Posted) + weightAssigned*p.Assigned + weightCommented*p.Commented})
	}
	evidence := evidenceOf(counted.Contributions, readable)
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if la, lb := len(evidence[a.ID]), len(evidence[b.ID]); la != lb {
			return la > lb
		}
		return a.ID < b.ID
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}

	// Names and pictures, by the chips' rule for people.
	refs := make([]entityLinkBusiness.Ref, 0, len(ranked))
	for _, p := range ranked {
		refs = append(refs, entityLinkBusiness.Ref{Kind: "person", ID: p.ID})
	}
	people, err := readMany(ctx, asker.Subject, refs)
	if err != nil {
		return nil, err
	}
	for i := range ranked {
		if i < len(people) && people[i].Access == entityLinkBusiness.AccessOK {
			ranked[i].Name = people[i].Title
			if k, ok := people[i].Data["avatar_key"].(string); ok {
				ranked[i].AvatarKey = k
			}
		}
		ev := evidence[ranked[i].ID]
		if len(ev) > EvidencePerPerson {
			ev = ev[:EvidencePerPerson]
		}
		ranked[i].Evidence = ev
		if ranked[i].Evidence == nil {
			ranked[i].Evidence = []Evidence{}
		}
	}
	out.People = append(out.People, ranked...)
	return out, nil
}

// uniqueSeeds is seeds each once, lower case, uuids only, at most n.
func uniqueSeeds(seeds []domain.Seed, n int) []domain.Seed {
	out := make([]domain.Seed, 0, min(len(seeds), n))
	seen := map[domain.Seed]bool{}
	for _, s := range seeds {
		if len(out) == n {
			break
		}
		id, err := uuid.Parse(s.ID)
		if err != nil {
			continue
		}
		s.ID = id.String()
		if s.Kind != domain.KindDoc && s.Kind != domain.KindTask && s.Kind != domain.KindMessage {
			continue
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// decide is the seeds the asker may open, with their titles, in one
// ReadMany (one read per kind).
func decide(ctx context.Context, who accessBusiness.Subject, seeds []domain.Seed) (map[domain.Seed]decided, error) {
	out := map[domain.Seed]decided{}
	if len(seeds) == 0 {
		return out, nil
	}
	refs := make([]entityLinkBusiness.Ref, 0, len(seeds))
	for _, s := range seeds {
		refs = append(refs, entityLinkBusiness.Ref{Kind: s.Kind, ID: s.ID})
	}
	got, err := readMany(ctx, who, refs)
	if err != nil {
		return nil, err
	}
	for i, e := range got {
		if i < len(seeds) && e.Access == entityLinkBusiness.AccessOK {
			out[seeds[i]] = decided{Title: e.Title, Data: linkData(seeds[i].Kind, e.Data)}
		}
	}
	return out, nil
}

// decided is a seed the asker may open: its title and what opening it needs.
type decided struct {
	Title string
	Data  map[string]any
}

// linkData is what the web app needs to open an item where it lives: a
// message's channel; nothing for a doc or a task, which open by their id.
func linkData(kind string, data map[string]any) map[string]any {
	if kind != domain.KindMessage {
		return nil
	}
	out := map[string]any{}
	for _, k := range []string{"channel_id", "channel_name"} {
		if v, ok := data[k]; ok {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

var roleRank = map[string]int{domain.RoleWrote: 0, domain.RoleAssigned: 1, domain.RoleCommented: 2}

// evidenceOf is each person's items, one entry an item (their strongest role
// on it), strongest role first, then in the order the read gave them. Only
// decided items.
func evidenceOf(cs []domain.Contribution, readable map[domain.Seed]decided) map[string][]Evidence {
	type key struct{ person, kind, item string }
	best := map[key]string{}
	var order []key
	for _, c := range cs {
		if _, ok := readable[domain.Seed{Kind: c.Kind, ID: c.Item}]; !ok {
			continue
		}
		k := key{c.Person, c.Kind, c.Item}
		was, seen := best[k]
		if !seen {
			order = append(order, k)
			best[k] = c.Role
		} else if roleRank[c.Role] < roleRank[was] {
			best[k] = c.Role
		}
	}
	out := map[string][]Evidence{}
	for _, k := range order {
		d := readable[domain.Seed{Kind: k.kind, ID: k.item}]
		out[k.person] = append(out[k.person], Evidence{Kind: k.kind, ID: k.item, Title: d.Title, Data: d.Data, Role: best[k]})
	}
	for p := range out {
		ev := out[p]
		sort.SliceStable(ev, func(i, j int) bool { return roleRank[ev[i].Role] < roleRank[ev[j].Role] })
	}
	return out
}

// Listed is whether person may be suggested (their own setting).
func Listed(ctx context.Context, person string) (bool, error) {
	id, err := uuid.Parse(person)
	if err != nil {
		return false, ErrBadRequest
	}
	return optOutModels.Listed(ctx, id)
}

// SetListed is person's own choice: listed (the default) or left out of
// every answer.
func SetListed(ctx context.Context, person string, listed bool) error {
	id, err := uuid.Parse(person)
	if err != nil {
		return ErrBadRequest
	}
	return optOutModels.SetListed(ctx, id, listed)
}
