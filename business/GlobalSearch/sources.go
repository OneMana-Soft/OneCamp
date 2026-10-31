package business

// The sources the search engine reads: one per index, each with who may find
// its entries (its reach), the fields words match, its title fields, and how
// each field of the one grammar compiles there (design/QUERY_STACK_CONTRACT.md
// §1: one place maps operators to each index).
//
// Each reach is today's access clause for that index, kept exactly: it is the
// privacy boundary, tested against the graph by the integration suite
// (doc_access_search_test.go, board_search_test.go, channel_search_test.go,
// demo_visitor_search_test.go, calendar_search_test.go). Every page is still
// checked against its source after the index answers (readableDocs.go,
// readableBoards.go, readableEvents.go, readableFiles.go, liveHits.go).

import (
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/akashc777/OneCamp/helpers"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
)

// Reach is what the asker reaches through membership: the sign-in's own
// lists of their channels, projects and teams.
type Reach struct {
	// Who is the asker as the Access Resolver names them, for the live
	// checks that decide as them (readableEvents.go).
	Who      accessBusiness.Subject
	UserUUID string
	Email    string
	Channels []string
	Projects []string
	Teams    []string
	// Calendars are the calendars whose events the asker finds besides their
	// own and their invitations, read once a search looks at events (nil
	// until then).
	Calendars *EventCalendars
}

// EventCalendars are the calendars whose events someone finds: every event on
// All's, and the events that aren't private on Open's (business/Calendar
// SearchCalendars, the grid's rule).
type EventCalendars struct {
	All  []string
	Open []string
}

// The kinds a search covers (type:'s values, the kinds of a search scope).
const (
	KindMessage = "message"
	KindDoc     = "doc"
	KindTask    = "task"
	KindProject = "project"
	KindFile    = "file"
	KindPerson  = "person"
	KindChannel = "channel"
	KindBoard   = "board"
	KindEvent   = "event"
	KindTeam    = "team"
)

// source is one index as the engine reads it.
type source struct {
	index string
	kind  string
	// boost lifts a whole source's hits, as the old query did (projects 50,
	// people 15).
	boost float64
	// reach is who may find its entries: a bool query, or nil for everyone.
	reach func(r Reach) map[string]any
	// text are the fields words match, with their boosts; prefix is the one a
	// word being typed matches the start of, with its boost.
	text        func(r Reach) []string
	prefix      []string
	prefixBoost float64
	// fuzzyNames lets a misspelt name match (projects, channels, teams,
	// people), as the old query let it.
	title []string
	// fields says which index fields hold each field's values here; a field
	// with none here can't be true of this source's entries.
	fields map[string][]string
	// dates are the date fields: created and updated.
	dates map[string]string
}

func fixed(fields ...string) func(Reach) []string { return func(Reach) []string { return fields } }

func term(field string, value any) map[string]any {
	return map[string]any{"term": map[string]any{field: value}}
}

func terms(field string, values []string) map[string]any {
	if values == nil {
		values = []string{}
	}
	return map[string]any{"terms": map[string]any{field: values}}
}

func anyOf(clauses ...map[string]any) map[string]any {
	return map[string]any{"bool": map[string]any{"should": clauses, "minimum_should_match": 1}}
}

func nestedTerm(path, field, value string) map[string]any {
	return map[string]any{"nested": map[string]any{"path": path, "ignore_unmapped": true, "query": term(field, value)}}
}

// spaces are the asker's projects and teams: the spaces whose docs they reach.
func spaces(r Reach) []string {
	return append(append([]string{}, r.Projects...), r.Teams...)
}

// The field ids every source may map. created and updated are dates; the
// rest are keyword fields holding ids.
const (
	fieldFrom      = "from"
	fieldTitle     = "title"
	inChannel      = "in.channel"
	inDM           = "in.dm"
	inProject      = "in.project"
	inTeam         = "in.team"
	fieldStatus    = "status"
	fieldPriority  = "priority"
	fieldAssignee  = "assignee"
	fieldProject   = "project"
	updatedQueried = "updated_date.q"
)

var sources = []source{
	{
		index: openSearchStruct.CHAT_INDEX, kind: KindMessage,
		reach: func(r Reach) map[string]any {
			return anyOf(term("chat_to_user_id", r.UserUUID), term("chat_by_user_id", r.UserUUID),
				nestedTerm("chat_participants", "chat_participants.user_uuid", r.UserUUID))
		},
		text:   fixed("chat_body", "chat_by_user_full_name", "chat_participants.user_name"),
		fields: map[string][]string{fieldFrom: {"chat_by_user_id"}, inDM: {openSearchStruct.ChatGroupingIDField}},
		dates:  map[string]string{"created": "created_date", "updated": updatedQueried},
	},
	{
		index: openSearchStruct.POST_INDEX, kind: KindMessage,
		reach:  func(r Reach) map[string]any { return terms("post_ch_id", r.Channels) },
		text:   fixed("post_body", "post_by_user_full_name"),
		fields: map[string][]string{fieldFrom: {"post_by_user_id.q"}, inChannel: {"post_ch_id"}},
		dates:  map[string]string{"created": "created_date", "updated": updatedQueried},
	},
	{
		index: openSearchStruct.COMMENT_INDEX, kind: KindMessage,
		reach: func(r Reach) map[string]any {
			u := r.UserUUID
			return anyOf(
				terms("comment_channel_id", r.Channels),
				term("comment_chat_to_user_id", u),
				term("comment_chat_from_user_id", u),
				nestedTerm("comment_chat_participants", "comment_chat_participants.user_uuid", u),
				terms("comment_project_id", r.Projects),
				term("comment_doc_created_by_user_id", u),
				term("comment_doc_reading_users", u),
				term("comment_doc_editing_users", u),
				term("comment_doc_commenting_users", u),
				term("comment_doc_private", false),
				term("comment_board_created_by_user_id", u),
				term("comment_board_reading_users", u),
				term("comment_board_editing_users", u),
				term("comment_board_commenting_users", u),
				term("comment_board_public", true),
			)
		},
		text: fixed("comment_body^3", "comment_by_user_full_name^2", "comment_chat_from_user_full_name^2", "comment_chat_participants.user_name^2",
			"comment_doc_title^1.5", "comment_project_name^1.5", "comment_channel_name^1.5"),
		prefix: []string{"comment_body"}, prefixBoost: 1,
		fields: map[string][]string{fieldFrom: {"comment_by_user_id.q"}, inChannel: {"comment_channel_id"}, inDM: {"comment_chat_grp_id"},
			inProject: {"comment_project_id"}},
		dates: map[string]string{"created": "created_date", "updated": updatedQueried},
	},
	{
		index: openSearchStruct.ATTACHMENT_INDEX, kind: KindFile,
		reach: func(r Reach) map[string]any {
			u := r.UserUUID
			return anyOf(
				terms("attachment_channel_id", r.Channels),
				term("attachment_chat_to_user_id", u),
				term("attachment_chat_from_user_id", u),
				nestedTerm("attachment_chat_participants", "attachment_chat_participants.user_uuid", u),
				terms("attachment_project_id", r.Projects),
				term("attachment_doc_created_by_user_id", u),
				term("attachment_doc_reading_users", u),
				term("attachment_doc_editing_users", u),
				term("attachment_doc_commenting_users", u),
				term("attachment_doc_private", false),
			)
		},
		text: fixed("attachment_file_name^3", "attachment_by_user_full_name", "attachment_chat_from_user_name", "attachment_chat_participants.user_name",
			"attachment_doc_title^1.5", "attachment_project_name^1.5", "attachment_channel_name^1.5"),
		prefix: []string{"attachment_file_name"}, prefixBoost: 1,
		title: []string{"attachment_file_name"},
		fields: map[string][]string{fieldFrom: {"attachment_by_user_id.q"}, inChannel: {"attachment_channel_id"}, inDM: {"attachment_chat_grp_id"},
			inProject: {"attachment_project_id"}},
		dates: map[string]string{"created": "created_date"},
	},
	{
		index: openSearchStruct.DOC_INDEX, kind: KindDoc,
		reach: func(r Reach) map[string]any {
			u := r.UserUUID
			return anyOf(
				term("doc_created_by_user_id", u),
				term("doc_reading_users", u),
				term("doc_editing_users", u),
				term("doc_commenting_users", u),
				term("doc_owner_user_id", u),
				term("doc_full_access_users", u),
				terms("doc_team_uuids", r.Teams),
				terms("doc_space_uuid", spaces(r)),
				term("doc_private", false),
			)
		},
		text:   fixed("doc_title^5", "doc_body", "doc_created_by_user_full_name^2"),
		prefix: []string{"doc_title"}, prefixBoost: 10,
		title:  []string{"doc_title"},
		fields: map[string][]string{fieldFrom: {"doc_created_by_user_id"}, inProject: {"doc_space_uuid"}, inTeam: {"doc_space_uuid", "doc_team_uuids"}},
		dates:  map[string]string{"created": "created_date", "updated": updatedQueried},
	},
	{
		index: openSearchStruct.BOARD_INDEX, kind: KindBoard,
		reach: func(r Reach) map[string]any {
			u := r.UserUUID
			return anyOf(
				term("board_created_by_user_id", u),
				term("board_reading_users", u),
				term("board_editing_users", u),
				term("board_commenting_users", u),
				term("board_private", false),
			)
		},
		text:   fixed("board_title^5", "board_snippet", "board_created_by_user_full_name^2"),
		prefix: []string{"board_title"}, prefixBoost: 10,
		title:  []string{"board_title"},
		fields: map[string][]string{fieldFrom: {"board_created_by_user_id"}},
		dates:  map[string]string{"created": "created_date", "updated": updatedQueried},
	},
	{
		index: openSearchStruct.TASK_INDEX, kind: KindTask,
		reach: func(r Reach) map[string]any {
			return anyOf(
				terms("task_project_id", r.Projects),
				term("task_assignee_user_id", r.UserUUID),
				map[string]any{"bool": map[string]any{
					"must":     []map[string]any{term("task_created_by_user_id", r.UserUUID)},
					"must_not": []map[string]any{{"exists": map[string]any{"field": "task_project_id"}}},
				}},
			)
		},
		text:   fixed("task_name^5", "task_desc", "task_assignee_user_full_name^2", "task_label^3"),
		prefix: []string{"task_name"}, prefixBoost: 10,
		title: []string{"task_name"},
		fields: map[string][]string{fieldFrom: {"task_created_by_user_id"}, inProject: {"task_project_id"}, fieldProject: {"task_project_id"},
			fieldStatus: {"task_status"}, fieldPriority: {"task_priority"}, fieldAssignee: {"task_assignee_user_id"}},
		dates: map[string]string{"created": "created_date", "updated": updatedQueried},
	},
	{
		index: openSearchStruct.EVENT_INDEX, kind: KindEvent,
		// Its organiser's and guests', and on a calendar the asker sees, all
		// of them or those that aren't private, as the grid shows them.
		reach: func(r Reach) map[string]any {
			var cals EventCalendars
			if r.Calendars != nil {
				cals = *r.Calendars
			}
			return anyOf(
				term("event_people", r.UserUUID),
				terms("event_calendar_id", cals.All),
				map[string]any{"bool": map[string]any{
					"filter":   []map[string]any{terms("event_calendar_id", cals.Open)},
					"must_not": []map[string]any{term("event_private", true)},
				}},
			)
		},
		text:   fixed("event_title^5", "event_notes", "event_location^2", "event_guest_names^2", "event_organiser_full_name^2"),
		prefix: []string{"event_title"}, prefixBoost: 10,
		title:  []string{"event_title"},
		fields: map[string][]string{fieldFrom: {"event_organiser_id"}, inProject: {"event_project_id"}},
		dates:  map[string]string{"created": "created_date", "updated": updatedQueried},
	},
	{
		index: openSearchStruct.PROJECT_INDEX, kind: KindProject, boost: 50,
		reach:  func(r Reach) map[string]any { return terms("project_id", r.Projects) },
		text:   fixed("project_name^5"),
		prefix: []string{"project_name"}, prefixBoost: 10,
		title:  []string{"project_name"},
		fields: map[string][]string{inTeam: {"project_team_id"}},
		dates:  map[string]string{"created": "created_date", "updated": updatedQueried},
	},
	{
		index: openSearchStruct.CHANNEL_INDEX, kind: KindChannel,
		reach:  func(r Reach) map[string]any { return terms("ch_id", r.Channels) },
		text:   fixed("ch_name^5"),
		prefix: []string{"ch_name"}, prefixBoost: 10,
		title: []string{"ch_name"},
		dates: map[string]string{"created": "created_date", "updated": updatedQueried},
	},
	{
		index: openSearchStruct.TEAM_INDEX, kind: KindTeam,
		reach:  func(r Reach) map[string]any { return terms("team_id", r.Teams) },
		text:   fixed("team_name^5"),
		prefix: []string{"team_name"}, prefixBoost: 10,
		title: []string{"team_name"},
		dates: map[string]string{"created": "created_date", "updated": updatedQueried},
	},
	{
		index: openSearchStruct.USER_INDEX, kind: KindPerson, boost: 15,
		// Everyone may find a person; the demo's shared visitor finds only the
		// demo's own people, after the index answers (demoPeopleHits).
		reach: nil,
		text: func(r Reach) []string {
			// People are found by their address as well as their name, except
			// by the demo's shared visitor, who is anyone at all.
			if helpers.IsDemoVisitor(r.Email) {
				return []string{"user_name^5", "user_full_name^5", "user_id"}
			}
			return []string{"user_name^5", "user_full_name^5", "user_email", "user_id"}
		},
		prefix: []string{"user_name", "user_full_name"}, prefixBoost: 2,
		title: []string{"user_name", "user_full_name"},
		dates: map[string]string{"created": "created_date", "updated": updatedQueried},
	},
}

// fuzzyNames are the sources whose names a misspelling still finds, as the
// old query let it (projects, channels, teams, people).
var fuzzyNames = map[string]bool{
	openSearchStruct.PROJECT_INDEX: true, openSearchStruct.CHANNEL_INDEX: true,
	openSearchStruct.TEAM_INDEX: true, openSearchStruct.USER_INDEX: true,
}

// indexKind is the kind each index's hits are.
func indexKind(index string) string {
	for _, s := range sources {
		if s.index == index {
			return s.kind
		}
	}
	return ""
}

// kindIndices are the indices that hold kinds.
func kindIndices(kinds []string) []string {
	want := map[string]bool{}
	for _, k := range kinds {
		want[strings.ToLower(k)] = true
	}
	var out []string
	for _, s := range sources {
		if want[s.kind] {
			out = append(out, s.index)
		}
	}
	return out
}
