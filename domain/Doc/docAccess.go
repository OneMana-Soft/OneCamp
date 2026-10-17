package domain

// What the Access Resolver reads of a doc for one person (docs programme,
// PROGRAM.md §4 #12; business/Doc docAccessDecide.go decides by it).
//
// Every read that decides who may do what with a doc carries the same facts,
// so every check decides alike: the doc's general access (privacy, comments
// and edits from everyone), its lock, its space, its deletion; its owner and
// maker; and, for the person, how it is shared with them, with a team they're
// in, and whether its space is one of theirs. A query takes them in two parts:
//
//	AccessPrelude(viewer) — blocks at the top of the query, which work out
//	    which projects and teams the person is in and which docs sit in
//	    their spaces (the space rule is one hop: the doc's own space only);
//	accessFacts(viewer) — fields of the doc block itself.
//
// viewer is how the query names the person: a variable holding their graph
// uid ("$userId", "$viewer"). The prelude's names start with "acc", which no
// other query in this package uses.

import (
	"context"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Doc"
)

// Nobody is a graph uid no node has. A read for someone the server can't
// name (an empty uid, which fails a whole query) decides as for a person with
// no grants, no teams and no spaces: what everyone in the workspace has.
const Nobody = "0xfffffffffffffff"

// ViewerUID is uid when it is a graph uid, else Nobody.
func ViewerUID(uid string) string {
	if helpers.IsDgraphUID(uid) {
		return uid
	}
	return Nobody
}

const (
	liveProjectFilter = `NOT gt(project_deleted_at, "1970-01-01T00:00:00Z")`
	liveTeamFilter    = `NOT gt(team_deleted_at, "1970-01-01T00:00:00Z")`
)

// AccessPrelude is the blocks that name the person (accV) and work out their
// spaces: the projects and teams they're a member or an admin of (not
// deleted), and the docs that sit in those spaces. accessFacts reads them, so
// a query has both or neither. viewer is a query variable holding the
// person's graph uid.
func AccessPrelude(viewer string) string {
	return accessPreludeFrom(`var(func: uid(` + viewer + `))`)
}

// accessPreludeByUUID is AccessPrelude for a query that names the person by
// uuid (viewer holds it).
func accessPreludeByUUID(viewer string) string {
	return accessPreludeFrom(`var(func: eq(user_uuid, ` + viewer + `))`)
}

func accessPreludeFrom(root string) string {
	return `
		accV as ` + root + ` {
			accPm as ~project_members
			accPa as ~project_admins
			accTm as ~team_members
			accTa as ~team_admins
		}
		var(func: uid(accPm, accPa)) @filter(` + liveProjectFilter + `) { accP as project_uuid }
		var(func: uid(accTm, accTa)) @filter(` + liveTeamFilter + `) { accT as team_uuid }
		var(func: eq(doc_space_uuid, val(accP))) @filter(eq(doc_space_kind, "project")) { accSP as doc_space_uuid }
		var(func: eq(doc_space_uuid, val(accT))) @filter(eq(doc_space_kind, "team")) { accST as doc_space_uuid }`
}

// accessScalars are the doc's own fields a decision reads.
const accessScalars = `
	doc_private
	doc_public_comment
	doc_public_edit
	doc_lock_level
	doc_space_kind
	doc_space_uuid
	doc_deleted_at
	doc_archived_at`

// accessOwner is the doc's owner, as a decision compares it; a block that
// names the doc's maker itself reads it with uid and user_uuid.
const accessOwner = `
	doc_owner { uid user_uuid }`

// accessCounts are the person's own facts, for the person the prelude names:
// how the doc is shared with them and with the teams they're in, and whether
// its space is theirs.
const accessCounts = `
	doc_read_access: count(doc_reading_users @filter(uid(accV)))
	doc_comment_access: count(doc_commenting_users @filter(uid(accV)))
	doc_edit_access: count(doc_editing_users @filter(uid(accV)))
	doc_full_access: count(doc_full_access_users @filter(uid(accV)))
	doc_team_view_access: count(doc_team_viewers @filter(` + inTeam + `))
	doc_team_comment_access: count(doc_team_commenters @filter(` + inTeam + `))
	doc_team_edit_access: count(doc_team_editors @filter(` + inTeam + `))
	doc_in_project_space: val(accSP)
	doc_in_team_space: val(accST)`

// inTeam is a team the person is in, as a member or an admin, not deleted.
const inTeam = `(uid_in(team_members, uid(accV)) OR uid_in(team_admins, uid(accV))) AND ` + liveTeamFilter

// accessFactsBut is every field of a doc a decision reads but its maker, for
// a block that reads the maker itself (with uid and user_uuid).
const accessFactsBut = accessScalars + accessOwner + accessCounts

// accessFacts is every field of a doc a decision for the person reads, for a
// block that names none of them itself.
const accessFacts = accessFactsBut + `
	doc_created_by { uid user_uuid }`

// DocAccessFacts is accessFactsBut for a query in another package (a
// comment's doc): the doc block's fields but its maker, which the block reads
// with uid and user_uuid; the query needs AccessPrelude too.
const DocAccessFacts = accessFactsBut

// GetDocAccessRows reads, for one person, what the Access Resolver decides
// each of the named docs by, deleted ones included (the decision says so):
// one query for the lot. The person is named by graph uid, or when there is
// none by uuid. Ids that aren't uuids are left out, and so is any doc that
// doesn't exist.
func GetDocAccessRows(ctx context.Context, docUUIDs []string, viewerUID, viewerUUID string) ([]*dgraphStruct.DgraphDoc, error) {
	quoted := quoteUUIDs(docUUIDs)
	if len(quoted) == 0 {
		return nil, nil
	}
	prelude, vars := AccessPrelude("$viewer"), map[string]string{"$viewer": ViewerUID(viewerUID)}
	if !helpers.IsDgraphUID(viewerUID) && viewerUUID != "" {
		prelude, vars = accessPreludeByUUID("$viewer"), map[string]string{"$viewer": viewerUUID}
	}
	q := `query access($viewer: string) {` + prelude + `
		docInfo(func: eq(doc_uuid, [` + strings.Join(quoted, ", ") + `])) {
			uid
			doc_uuid` + accessFacts + `
		}
	}`
	docs, err := dgraphModels.GetDgraphDocs(ctx, q, vars)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDocAccessRows err: %+v", err)
	}
	return docs, err
}

// DocACL is everything a doc is shared with, for the sharing panel and for
// working out what a change would do: its people at each level, its teams
// with their members, its space's members, its general access and its lock.
type DocACL struct {
	Doc *dgraphStruct.DgraphDoc
	// SpaceMembers are who is in the doc's space (PeopleOf its members and
	// admins), when it has one: a project's or a team's.
	SpaceMembers []string
	// SpaceName is that project's or team's name.
	SpaceName string
}

// aclPerson is how the panel names a person.
const aclPerson = `{ uid user_uuid user_name user_full_name user_profile_object_key }`

// aclTeam is a team the doc is shared with, and who is in it.
const aclTeam = `@filter(` + liveTeamFilter + `) { uid team_uuid team_name team_members { uid user_uuid } team_admins { uid user_uuid } }`

// GetDocACL reads a doc's whole sharing, deleted or not. ErrNotFound for no doc.
func GetDocACL(ctx context.Context, docUUID string) (*DocACL, error) {
	q := fmt.Sprintf(`query acl($id: string) {
		var(func: eq(doc_uuid, $id)) { accSpace as doc_space_uuid }
		docInfo(func: eq(doc_uuid, $id)) {
			uid
			doc_uuid
			doc_title%s
			doc_locked_at
			doc_locked_by %s
			doc_created_by %s
			doc_owner %s
			doc_full_access_users %s
			doc_editing_users %s
			doc_commenting_users %s
			doc_reading_users %s
			doc_team_editors %s
			doc_team_commenters %s
			doc_team_viewers %s
		}`+spaceBlocks("val(accSpace)")+`
	}`, accessScalars, aclPerson, aclPerson, aclPerson, aclPerson, aclPerson, aclPerson, aclPerson, aclTeam, aclTeam, aclTeam)
	var out struct {
		DocInfo []*dgraphStruct.DgraphDoc `json:"docInfo"`
		spaceRead
	}
	if err := dgraphInit.ReadInto(ctx, q, map[string]string{"$id": docUUID}, &out); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDocACL err: %+v", err)
		return nil, err
	}
	if len(out.DocInfo) == 0 || out.DocInfo[0] == nil || out.DocInfo[0].Uuid == "" {
		return nil, ErrNotFound
	}
	acl := &DocACL{Doc: out.DocInfo[0]}
	acl.SpaceName, acl.SpaceMembers, _ = out.of(acl.Doc.SpaceKind)
	return acl, nil
}

// TeamMembers reads teams (by uuid, not deleted) with the uuids of their
// members and admins: who a grant to each would reach. A team that isn't there
// is left out.
func TeamMembers(ctx context.Context, teamUUIDs []string) ([]*dgraphStruct.DgraphTeam, error) {
	quoted := quoteUUIDs(teamUUIDs)
	if len(quoted) == 0 {
		return nil, nil
	}
	q := `{
		team(func: eq(team_uuid, [` + strings.Join(quoted, ", ") + `])) @filter(` + liveTeamFilter + `) {
			team_uuid
			team_name
			team_members { user_uuid }
			team_admins { user_uuid }
		}
	}`
	var out struct {
		Team []*dgraphStruct.DgraphTeam `json:"team"`
	}
	if err := dgraphInit.ReadInto(ctx, q, nil, &out); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/TeamMembers err: %+v", err)
		return nil, err
	}
	return out.Team, nil
}

// PeopleByUUID reads people (by uuid) as a sharing panel names them, keyed by
// lower-case uuid; anyone not there is left out.
func PeopleByUUID(ctx context.Context, userUUIDs []string) (map[string]*dgraphStruct.DgraphUser, error) {
	quoted := quoteUUIDs(userUUIDs)
	out := map[string]*dgraphStruct.DgraphUser{}
	if len(quoted) == 0 {
		return out, nil
	}
	var got struct {
		People []*dgraphStruct.DgraphUser `json:"people"`
	}
	q := `{ people(func: eq(user_uuid, [` + strings.Join(quoted, ", ") + `])) ` + aclPerson + ` }`
	if err := dgraphInit.ReadInto(ctx, q, nil, &got); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/PeopleByUUID err: %+v", err)
		return nil, err
	}
	for _, p := range got.People {
		if p != nil && p.Uuid != "" {
			out[strings.ToLower(p.Uuid)] = p
		}
	}
	return out, nil
}

// DocTitles reads docs' titles (by uuid), keyed by lower-case uuid, for the
// people who may open them; a doc not there is left out.
func DocTitles(ctx context.Context, docUUIDs []string) (map[string]string, error) {
	quoted := quoteUUIDs(docUUIDs)
	out := map[string]string{}
	if len(quoted) == 0 {
		return out, nil
	}
	docs, err := dgraphModels.GetDgraphDocs(ctx, `{
		docInfo(func: eq(doc_uuid, [`+strings.Join(quoted, ", ")+`])) { doc_uuid doc_title }
	}`, map[string]string{})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/DocTitles err: %+v", err)
		return nil, err
	}
	for _, d := range docs {
		if d != nil && d.Uuid != "" {
			out[strings.ToLower(d.Uuid)] = d.Title
		}
	}
	return out, nil
}
