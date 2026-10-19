package business

// The core fields (design/QUERY_STACK_CONTRACT.md §4.7), which every area may
// use, and how their names resolve: people by @handle, places by #channel or
// by the person a DM is with, each only as far as the asker may see.
//
// Each lookup is a seam, so the rules are tested without the databases. What
// resolves here is what C2-0 needs; people by full name, and projects, teams,
// spaces, docs, tables and group DMs by name, resolve when the search engine
// lands on the grammar (docs C2-1). Until then those names go back to being
// words in a search, and are an issue in a list.

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	querySpec "github.com/akashc777/OneCamp/business/QuerySpec"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// The kinds of thing a search covers (type:), with the words people use for
// them. projects adds its task types and synonyms (bug, epic, …).
var kindChoices = []Choice{
	{Value: "message", Synonyms: []string{"messages", "chat", "chats", "post", "posts", "reply", "replies", "comment", "comments"}, Label: "Messages"},
	{Value: "doc", Synonyms: []string{"docs", "page", "pages"}, Label: "Docs"},
	{Value: "task", Synonyms: []string{"tasks", "issue", "issues", "story", "stories", "ticket", "tickets", "todo", "todos"}, Label: "Tasks"},
	{Value: "project", Synonyms: []string{"projects"}, Label: "Projects"},
	{Value: "file", Synonyms: []string{"files", "attachment", "attachments"}, Label: "Files"},
	{Value: "person", Synonyms: []string{"people", "user", "users", "member", "members"}, Label: "People"},
	{Value: "channel", Synonyms: []string{"channels"}, Label: "Channels"},
	{Value: "board", Synonyms: []string{"boards", "whiteboard", "whiteboards"}, Label: "Boards"},
	{Value: "team", Synonyms: []string{"teams"}, Label: "Teams"},
}

func init() { registerCore(Fields) }

// registerCore registers the core fields into r.
func registerCore(r *registry) {
	r.Register(Field{Name: "text", Family: FamilyText, Help: "Words anywhere"})
	r.Register(Field{Name: "title", Family: FamilyText, Sortable: true, Help: "Words in titles and names only"})
	r.Register(Field{Name: "type", Family: FamilyKind, Choices: kindChoices, Help: "What kind of thing: message, doc, task, file, person…"})
	r.Register(Field{Name: "in", Family: FamilyPlace, Resolve: resolvePlace, Label: labelPlace, Help: "In a channel (#launch) or a DM (@sam)"})
	r.Register(Field{Name: "from", Aliases: []string{"creator", "author", "by", "reporter"}, Family: FamilyPeople, Resolve: resolvePerson, Label: labelPerson, Help: "Written or made by someone (@sam, me)"})
	r.Register(Field{Name: "created", Family: FamilyDate, Sortable: true, Help: "When it was made"})
	r.Register(Field{Name: "updated", Aliases: []string{"edited", "modified"}, Family: FamilyDate, Sortable: true, Help: "When it last changed"})
	r.Register(Field{Name: "has", Family: FamilyFlag, Help: "Has something: a link, a file, a reaction…"})
	r.Register(Field{Name: "is", Family: FamilyFlag, Help: "Is something: a thread, saved, verified…"})
	r.Register(Field{Name: "before", Family: FamilyDate, Target: "created", Fixed: querySpec.OperatorBefore, Help: "Made before a day (not on it)"})
	r.Register(Field{Name: "after", Family: FamilyDate, Target: "created", Fixed: querySpec.OperatorAfter, Help: "Made after a day (not on it)"})
	r.Register(Field{Name: "on", Family: FamilyDate, Target: "created", Fixed: querySpec.OperatorIs, Help: "Made on a day"})
	r.Register(Field{Name: "during", Family: FamilyDate, Target: "created", Fixed: querySpec.OperatorWithin, Help: "Made during a month or a year"})
}

// The lookups, seams for tests.
var (
	userIDByHandle = func(ctx context.Context, handle string) (string, error) {
		id, err := userDomain.GetActiveUserIDByHandle(ctx, handle)
		if err != nil || id == uuid.Nil {
			return "", err
		}
		return id.String(), nil
	}
	// userEmail is a person's address and whether they're a bot, for the
	// demo's visitor; ok is false for nobody.
	userEmail = func(ctx context.Context, id string) (email string, isBot bool, ok bool, err error) {
		uid, perr := uuid.Parse(id)
		if perr != nil {
			return "", false, false, nil
		}
		u, err := userDomain.GetActiveUserByUUID(ctx, uid)
		if err != nil || u == nil {
			return "", false, false, err
		}
		return u.EmailID, u.IsBot, true, nil
	}
	// channelIDByName is the live channel called name, or "" for none. No
	// such channel is an answer, never an error: an error here would read
	// differently from a private channel the asker can't open, and tell them
	// which names exist.
	channelIDByName = func(ctx context.Context, name string) (string, error) {
		ch, err := channelDomain.GetChannelByName(ctx, name)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		if err != nil || ch == nil {
			return "", err
		}
		return ch.Id.String(), nil
	}
	// readable is which of the resources the asker may read: the Access
	// Resolver's answer, the only one (a channel they can't read is left out
	// exactly as one that isn't there).
	readable = func(ctx context.Context, who accessBusiness.Subject, res []accessBusiness.Resource) ([]accessBusiness.Resource, error) {
		return accessBusiness.Rules.Filter(ctx, who, res, accessBusiness.LevelView)
	}
)

// person is the person a typed @handle names, as the asker may see them.
func person(ctx context.Context, who Asker, typed string) (string, error) {
	handle := strings.TrimPrefix(strings.TrimSpace(typed), "@")
	if handle == "" || !strings.HasPrefix(strings.TrimSpace(typed), "@") {
		return "", nil
	}
	id, err := userIDByHandle(ctx, handle)
	if err != nil || id == "" {
		return "", err
	}
	if helpers.IsDemoVisitor(who.Email) {
		email, isBot, ok, err := userEmail(ctx, id)
		if err != nil {
			return "", err
		}
		if !ok || !helpers.DemoListsPerson(who.Email, email, isBot) {
			return "", nil
		}
	}
	return id, nil
}

// resolvePerson is from:'s values: a person by @handle. me and @my_team are
// read before any lookup (Build).
func resolvePerson(ctx context.Context, who Asker, _ querySpec.Scope, typed string) ([]querySpec.Value, error) {
	id, err := person(ctx, who, typed)
	if err != nil || id == "" {
		return nil, err
	}
	return []querySpec.Value{{ID: id, Type: "user", Label: "@" + strings.TrimPrefix(strings.TrimSpace(typed), "@")}}, nil
}

// resolvePlace is in:'s values: a channel by #name, when the asker may read
// it, or the DM with a person by @handle.
func resolvePlace(ctx context.Context, who Asker, _ querySpec.Scope, typed string) ([]querySpec.Value, error) {
	t := strings.TrimSpace(typed)
	switch {
	case strings.HasPrefix(t, "#"):
		name := strings.TrimPrefix(t, "#")
		if name == "" {
			return nil, nil
		}
		id, err := channelIDByName(ctx, name)
		if err == nil && id == "" && strings.ToLower(name) != name {
			id, err = channelIDByName(ctx, strings.ToLower(name))
		}
		if err != nil || id == "" {
			return nil, err
		}
		kept, err := readable(ctx, who.Subject, []accessBusiness.Resource{{Kind: "channel", UUID: id}})
		if err != nil || len(kept) == 0 {
			return nil, err
		}
		return []querySpec.Value{{ID: id, Type: "channel", Label: "#" + name}}, nil
	case strings.HasPrefix(t, "@"):
		id, err := person(ctx, who, t)
		if err != nil || id == "" {
			return nil, err
		}
		// The DM is the reader's own with that person: the value names the
		// person (TypeDMWith), so a saved view means each reader's DM, and a
		// compiler finds it with helpers.GetGroupingId(reader, person).
		return []querySpec.Value{{ID: id, Type: TypeDMWith, Label: t}}, nil
	}
	return nil, nil
}

// TypeDMWith is a place value naming the person a DM is with: the reader's
// DM with them.
const TypeDMWith = "dm_with"

// The lookups Format makes, seams for tests.
var (
	handleOf = func(ctx context.Context, id string) (string, error) {
		uid, err := uuid.Parse(id)
		if err != nil {
			return "", nil
		}
		return userDomain.GetHandle(ctx, uid)
	}
	channelNames = func(ctx context.Context, ids []string) (map[string]string, error) {
		return channelDomain.GetChannelNamesByUUIDs(ctx, ids)
	}
)

// labelPerson is a person as the asker reads them: @handle, when they may see
// the person.
func labelPerson(ctx context.Context, who Asker, _ querySpec.Scope, v querySpec.Value) (string, bool) {
	if helpers.IsDemoVisitor(who.Email) {
		email, isBot, ok, err := userEmail(ctx, v.ID)
		if err != nil || !ok || !helpers.DemoListsPerson(who.Email, email, isBot) {
			return "", false
		}
	}
	handle, err := handleOf(ctx, v.ID)
	if err != nil || handle == "" {
		return "", false
	}
	return "@" + handle, true
}

// labelPlace is a place as the asker reads it: #name for a channel they may
// read, @handle for a DM.
func labelPlace(ctx context.Context, who Asker, scope querySpec.Scope, v querySpec.Value) (string, bool) {
	switch v.Type {
	case TypeDMWith:
		return labelPerson(ctx, who, scope, v)
	case "channel":
		kept, err := readable(ctx, who.Subject, []accessBusiness.Resource{{Kind: "channel", UUID: v.ID}})
		if err != nil || len(kept) == 0 {
			return "", false
		}
		names, err := channelNames(ctx, []string{v.ID})
		if err != nil || names[v.ID] == "" {
			return "", false
		}
		return "#" + names[v.ID], true
	}
	return "", false
}
