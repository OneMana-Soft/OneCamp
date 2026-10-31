package business

import (
	"context"
	"fmt"
	"strings"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	importDomain "github.com/akashc777/OneCamp/domain/Import"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	conversationModels "github.com/akashc777/OneCamp/models/postgres/Conversation"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// UserResolveStats are returned alongside the resolution pass.
type UserResolveStats struct {
	Total      int `json:"total"`
	Reused     int `json:"reused"`
	CreatedExt int `json:"created_external"`
	Bots       int `json:"bots"`
	Skipped    int `json:"skipped"`
}

// runUsersStage walks every SourceUser the provider streams and ensures
// each has a OneCamp users row. Idempotent: re-runs against the same
// import_id are no-ops thanks to the import_id_map upsert, and re-runs
// against the same workspace are no-ops thanks to the workspace map.
//
// Matching policy (personFacts.match, first match wins), else a new external
// user (synth email if none provided).
func runUsersStage(ctx context.Context, prov importProvider.Provider,
	job *importModels.Job, opts importProvider.JobOptions) error {

	userCh, errCh := prov.IterUsers(ctx, job, opts)

	stats := UserResolveStats{}
	members := &memberNames{}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err, ok := <-errCh:
			if ok && err != nil {
				return err
			}
			// errCh closed without err; continue draining userCh.
			errCh = nil
		case su, ok := <-userCh:
			if !ok {
				patch := mustMarshal(map[string]interface{}{
					"users":             stats,
					"users_resolved_at": time.Now().UTC(),
				})
				_ = importModels.UpdateProgress(ctx, job.Id, patch)
				return nil
			}
			stats.Total++
			if err := resolveOneUser(ctx, job, &su, &stats, members); err != nil {
				// Resolution should be tolerant of single-user failures.
				// Redact email from the message so privacy-sensitive
				// data isn't forwarded to log aggregators (the per-import
				// id_map metadata still carries the address for operator
				// inspection inside OneCamp's admin UI).
				importModels.LogImportError(ctx, job.Id, nil,
					importModels.EntityUser, su.SourceID,
					importModels.SeverityError, "USER_RESOLVE_FAILED",
					redactEmail(err.Error()), nil)
				stats.Skipped++
			}
		}
	}
}

// personMatch is who a source's person is here, and how the import knows:
// by its own map, the workspace's map from an earlier import, their email or
// their name. by "" is no one, and they come as a placeholder; sameName is
// how many members share their name when that is why.
type personMatch struct {
	by       string
	id       uuid.UUID
	sameName int
}

// The ways a person is matched (import_id_map's matched_by).
const (
	matchedByImport    = "import"
	matchedByWorkspace = "workspace_map"
	matchedByEmail     = "email"
	matchedByName      = "name"
)

// personFacts is what matching people reads, for many at once: the import's
// own map, the workspace's map from an earlier import of the same source, the
// members by address, and the members by name.
type personFacts struct {
	own, earlier map[string]uuid.UUID // by source id
	byAddress    map[string]uuid.UUID // by address in lower case
	members      *memberNames
}

// readPersonFacts reads what matching people needs, one statement a kind: the
// maps by the people's source ids, the members by their addresses, and the
// members' names when someone is placed by none of those.
func readPersonFacts(ctx context.Context, job *importModels.Job, people []importProvider.SourceUser, members *memberNames) (*personFacts, error) {
	ids := make([]string, 0, len(people))
	var addresses []string
	for i := range people {
		ids = append(ids, people[i].SourceID)
		if a := addressOf(&people[i]); a != "" {
			addresses = append(addresses, a)
		}
	}
	f := &personFacts{members: members}
	var err error
	if f.own, err = importDomain.LookupIdMappingsBatch(ctx, job.Id, importModels.EntityUser, ids); err != nil {
		return nil, err
	}
	if f.earlier, err = importModels.LookupWorkspaceMappingsBatch(ctx, job.Provider, job.SourceWorkspaceName, importModels.EntityUser, ids); err != nil {
		return nil, err
	}
	if f.byAddress, err = userBusiness.MembersByAddress(ctx, addresses); err != nil {
		return nil, err
	}
	// Someone matched before is never looked for by name, so the members
	// aren't read for them.
	byName := false
	for i := range people {
		if _, placed := f.placed(&people[i]); !placed {
			byName = true
			break
		}
	}
	if byName {
		if err := members.load(ctx); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// match finds who su is here, in this order: the import's own map; the
// workspace's map from an earlier import of the same source; the member with
// su's address; then, when the source gave no address (Trello, a CSV's
// names), the one member with su's name. Two members with one name are never
// guessed between, and a bot is never matched by name.
func (f *personFacts) match(su *importProvider.SourceUser) personMatch {
	if m, placed := f.placed(su); placed {
		return m
	}
	ids := f.members.named(su.DisplayName)
	if len(ids) != 1 {
		return personMatch{sameName: len(ids)}
	}
	return personMatch{by: matchedByName, id: ids[0]}
}

// placed is match's answer up to the members' names, and whether it is the
// answer: no map and no address leave someone who isn't a bot to their name.
func (f *personFacts) placed(su *importProvider.SourceUser) (personMatch, bool) {
	if id := f.own[su.SourceID]; id != uuid.Nil {
		return personMatch{by: matchedByImport, id: id}, true
	}
	if id := f.earlier[su.SourceID]; id != uuid.Nil {
		return personMatch{by: matchedByWorkspace, id: id}, true
	}
	if a := addressOf(su); a != "" {
		if id := f.byAddress[a]; id != uuid.Nil {
			return personMatch{by: matchedByEmail, id: id}, true
		}
		return personMatch{}, true
	}
	return personMatch{}, su.IsBot
}

// addressOf is su's address as addresses are compared.
func addressOf(su *importProvider.SourceUser) string {
	return strings.TrimSpace(strings.ToLower(su.Email))
}

// memberNames are the workspace's members by name (PersonNameKey of their
// display and full names) and each one's name as people see it, read once a
// pass, when a person first needs them.
type memberNames struct {
	byName map[string][]uuid.UUID
	nameOf map[uuid.UUID]string
}

// load reads the members' names from the graph, keeping those Postgres says
// are members: live, and not external rows or bots.
func (m *memberNames) load(ctx context.Context) error {
	if m.byName != nil {
		return nil
	}
	users, _, err := userDomain.GetMembersPage(ctx, dgraphquery.ReadAllMax, 0, userDomain.MembersFilter{})
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, 0, len(users))
	for _, u := range users {
		if id, err := uuid.Parse(u.Uuid); err == nil {
			ids = append(ids, id)
		}
	}
	people, err := conversationModels.People(ctx, ids)
	if err != nil {
		return err
	}
	m.byName, m.nameOf = membersByName(users, func(id uuid.UUID) bool {
		p, ok := people[id]
		return ok && p.Active && !p.External && !p.Bot
	})
	return nil
}

// named is the members called name: one is a match, two or more are a
// shared name.
func (m *memberNames) named(name string) []uuid.UUID {
	return m.byName[helpers.PersonNameKey(name)]
}

// membersByName indexes the users member says are members by each of their
// names' keys, once a user for a name, and gives each one's name as people
// see it. Pure.
func membersByName(users []*dgraphStruct.DgraphUser, member func(uuid.UUID) bool) (byName map[string][]uuid.UUID, nameOf map[uuid.UUID]string) {
	byName = make(map[string][]uuid.UUID, len(users))
	nameOf = make(map[uuid.UUID]string, len(users))
	for _, u := range users {
		id, err := uuid.Parse(u.Uuid)
		if err != nil || !member(id) {
			continue
		}
		nameOf[id] = u.DisplayName()
		seen := ""
		for _, n := range []string{u.UserName, u.UserFullName} {
			if k := helpers.PersonNameKey(n); k != "" && k != seen {
				byName[k] = append(byName[k], id)
				seen = k
			}
		}
	}
	return byName, nameOf
}

// previewPeople says how the users stage will match each person a plan lists
// (personFacts.match), and whom, reading only.
func previewPeople(ctx context.Context, job *importModels.Job, people []importProvider.PlanPerson) error {
	if len(people) == 0 {
		return nil
	}
	members := &memberNames{}
	if err := members.load(ctx); err != nil {
		return err
	}
	users := make([]importProvider.SourceUser, len(people))
	for i, p := range people {
		users[i] = importProvider.SourceUser{SourceID: p.SourceID, Email: p.Email, DisplayName: p.Name}
	}
	facts, err := readPersonFacts(ctx, job, users, members)
	if err != nil {
		return err
	}
	for i := range people {
		m := facts.match(&users[i])
		people[i].MatchedBy, people[i].SameName, people[i].Member = m.by, m.sameName, members.nameOf[m.id]
	}
	return nil
}

func resolveOneUser(ctx context.Context, job *importModels.Job,
	su *importProvider.SourceUser, stats *UserResolveStats, members *memberNames) error {

	if su.SourceID == "" {
		stats.Skipped++
		return nil
	}
	facts, err := readPersonFacts(ctx, job, []importProvider.SourceUser{*su}, members)
	if err != nil {
		return err
	}
	m := facts.match(su)
	switch m.by {
	case matchedByImport:
		stats.Reused++
		return nil
	case matchedByWorkspace, matchedByEmail, matchedByName:
		meta := map[string]any{"matched_by": m.by}
		if m.by == matchedByEmail {
			meta["email"] = addressOf(su)
		}
		if err := importModels.RecordMapping(ctx, job.Id, job.Provider, job.SourceWorkspaceName,
			importModels.EntityUser, su.SourceID, m.id, nil, mustMarshal(meta), false); err != nil {
			return err
		}
		stats.Reused++
		return nil
	}

	email := addressOf(su)
	// No one here: a placeholder, an external user.
	emailToUse := email
	displayName := pickDisplayName(su)
	login := su.Login
	if login == "" {
		login = displayName
	}
	if login == "" {
		login = su.SourceID
	}
	// Prefix with provider so the external login can't collide with a
	// real GitHub login (login uniqueness is workspace-scoped on Slack
	// import too — same approach).
	prefixedLogin := job.Provider + "-" + sanitizeLogin(login)

	if emailToUse == "" {
		emailToUse = synthEmail(job.Provider, job.SourceWorkspaceName, su.SourceID)
	}
	// If the synth or real email collides with another user (rare, but
	// real on workspaces with split identities), suffix.
	if existing, _ := userBusiness.GetUserByEmailId(ctx, &emailToUse); existing != nil {
		emailToUse = collisionSuffix(emailToUse, su.SourceID)
	}

	newUser, err := userBusiness.CreateExternalGitHubUser(
		ctx,
		prefixedLogin,
		displayName,
		su.AvatarURL, // raw URL; CreateExternalGitHubUser stores it on profile_object_key
		"",           // no html_url for non-GitHub
		emailToUse,
	)
	if err != nil {
		return fmt.Errorf("create external user: %w", err)
	}

	if err := importModels.RecordMapping(ctx, job.Id, job.Provider, job.SourceWorkspaceName,
		importModels.EntityUser, su.SourceID, newUser.Id, nil,
		mustMarshal(placeholderMetadata(*su, emailToUse, displayName, prefixedLogin)), true); err != nil {
		return err
	}

	if su.IsBot {
		stats.Bots++
	} else {
		stats.CreatedExt++
	}
	return nil
}

func pickDisplayName(u *importProvider.SourceUser) string {
	if v := strings.TrimSpace(u.DisplayName); v != "" {
		return v
	}
	if v := strings.TrimSpace(u.Login); v != "" {
		return v
	}
	return u.SourceID
}

func synthEmail(provider, workspaceName, sourceId string) string {
	ws := slugify(workspaceName)
	if ws == "" {
		ws = provider
	}
	return fmt.Sprintf("%s-import-%s-%s@no-reply.local",
		provider, ws, strings.ToLower(sanitizeLogin(sourceId)))
}

func collisionSuffix(email, sourceId string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return email + "+x-" + strings.ToLower(sanitizeLogin(sourceId))
	}
	return email[:at] + "+x-" + strings.ToLower(sanitizeLogin(sourceId)) + email[at:]
}

func slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		case !prevDash && b.Len() > 0:
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func sanitizeLogin(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'),
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-_.")
	if out == "" {
		return "user"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// people is a chunk's memo of who source ids came across as: a person is
// resolved (two statements and a graph read) once a chunk, where it was once
// for each task, subtask and comment they made or were given.
type people struct {
	job  *importModels.Job
	seen map[string]personRef
}

type personRef struct {
	match importModels.PersonMatch
	info  *userModels.UserInfo
}

func newPeople(job *importModels.Job) *people {
	return &people{job: job, seen: make(map[string]personRef, 16)}
}

// resolve is resolveUser, once a person.
func (p *people) resolve(ctx context.Context, sourceId string) (importModels.PersonMatch, *userModels.UserInfo) {
	if r, ok := p.seen[sourceId]; ok {
		return r.match, r.info
	}
	match, info := resolveUser(ctx, p.job, sourceId)
	p.seen[sourceId] = personRef{match, info}
	return match, info
}

// resolveUser is who a source's person is here, and how they came across
// (PeopleMatched), read in one statement before their user.
func resolveUser(ctx context.Context, job *importModels.Job, sourceId string) (importModels.PersonMatch, *userModels.UserInfo) {
	if sourceId == "" {
		return importModels.PersonMatch{}, nil
	}
	matched, err := importModels.PeopleMatched(ctx, job.Id, job.Provider, job.SourceWorkspaceName, []string{sourceId})
	match := matched[sourceId]
	if err != nil || match.ID == uuid.Nil {
		return importModels.PersonMatch{}, nil
	}
	pgUser, err := userBusiness.GetUserByUUID(ctx, match.ID)
	if err != nil || pgUser == nil {
		return match, nil
	}
	dgUser, err := userBusiness.GetDgraphUserInfoByUUID(ctx, match.ID.String())
	if err != nil || dgUser == nil {
		return match, &userModels.UserInfo{UserPostgresInfo: *pgUser}
	}
	return match, &userModels.UserInfo{
		UserPostgresInfo: *pgUser,
		UserDgraphInfo:   *dgUser,
	}
}

// placeholderMetadata is what the import records about a person it made a
// placeholder for: people.go reads is_bot and deleted to leave bots and
// people who left the source out of the invitations. Pure.
func placeholderMetadata(su importProvider.SourceUser, email, displayName, login string) map[string]any {
	return map[string]any{
		"matched_by":   "external",
		"email":        email,
		"display_name": displayName,
		"login":        login,
		"is_bot":       su.IsBot,
		"deleted":      su.Left,
	}
}
