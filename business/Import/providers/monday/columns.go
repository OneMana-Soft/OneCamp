package monday

// monday.com keeps everything except an item's name in typed columns.
// Every column_value comes back with `text` (the rendered string) and
// `value` (a JSON string whose shape depends on the column type):
//
//	status    text "Working on it"         value {"index":0,"post_id":null,"changed_at":"…"}
//	date      text "2026-10-08 14:00"      value {"date":"2026-10-08","time":"14:00:00","changed_at":"…"}
//	timeline  text "2026-10-01 - 2026-10-10" value {"from":"2026-10-01","to":"2026-10-10","changed_at":"…"}
//	people    text "Ada, Grace"            value {"personsAndTeams":[{"id":4012,"kind":"person"}],"changed_at":"…"}
//	tags      text "bug, ui"               value {"tag_ids":[12,13]}
//
// Status / priority / labels are read from `text` (the label is not in
// `value`, only its palette index). Dates and people are read from
// `value` first, because `text` is rendered in the account's timezone
// and people's `text` carries names rather than ids.
//
// Which column means what is a convention, not a schema: a board can
// have three status columns and two date columns. The picks below are
// title-driven ("Priority", "Due date", "Owner") with a first-of-type
// fallback, and everything not picked is preserved in the description
// so no data silently disappears.

import (
	"encoding/json"
	"html"
	"sort"
	"strings"
	"time"
	"unicode"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/helpers"
)

// ─── DTOs (wire shapes) ─────────────────────────────────────────────

type mondayColumnValue struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Text   *string         `json:"text"`
	Value  json.RawMessage `json:"value"`
	Column *struct {
		Title string `json:"title"`
	} `json:"column"`
}

func (cv mondayColumnValue) text() string {
	if cv.Text == nil {
		return ""
	}
	return strings.TrimSpace(*cv.Text)
}

func (cv mondayColumnValue) title() string {
	if cv.Column != nil && cv.Column.Title != "" {
		return cv.Column.Title
	}
	return cv.ID
}

// kind normalises monday's column type, folding the legacy names the
// API still emits on older boards ("color", "multiple-person",
// "timerange") onto the current ones.
func (cv mondayColumnValue) kind() string {
	switch t := strings.ToLower(cv.Type); t {
	case "color", "status":
		return "status"
	case "multiple-person", "person", "people":
		return "people"
	case "timerange", "timeline":
		return "timeline"
	case "tag", "tags":
		return "tags"
	case "date":
		return "date"
	case "dropdown":
		return "dropdown"
	case "file":
		return "file"
	default:
		return t
	}
}

// valueJSON unwraps `value`. monday's JSON scalar arrives as a string
// containing JSON; some clients/versions hand back the object directly.
func (cv mondayColumnValue) valueJSON() []byte {
	raw := cv.Value
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || s == "" || s == "null" {
			return nil
		}
		return []byte(s)
	}
	return raw
}

type mondayAsset struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	PublicURL     string `json:"public_url"`
	FileExtension string `json:"file_extension"`
	FileSize      int64  `json:"file_size"`
}

type mondayReply struct {
	ID        string  `json:"id"`
	Body      string  `json:"body"`
	TextBody  string  `json:"text_body"`
	CreatedAt *string `json:"created_at"`
	CreatorID *string `json:"creator_id"`
}

type mondayUpdate struct {
	ID        string        `json:"id"`
	Body      string        `json:"body"`
	TextBody  string        `json:"text_body"`
	CreatedAt *string       `json:"created_at"`
	CreatorID *string       `json:"creator_id"`
	Assets    []mondayAsset `json:"assets"`
	Replies   []mondayReply `json:"replies"`
}

type mondayGroup struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type mondayItem struct {
	ID           string              `json:"id"`
	Name         string              `json:"name"`
	State        string              `json:"state"`
	URL          string              `json:"url"`
	CreatedAt    *string             `json:"created_at"`
	UpdatedAt    *string             `json:"updated_at"`
	CreatorID    *string             `json:"creator_id"`
	Group        *mondayGroup        `json:"group"`
	ColumnValues []mondayColumnValue `json:"column_values"`
	Assets       []mondayAsset       `json:"assets"`
	Updates      []mondayUpdate      `json:"updates"`
	Subitems     []mondayItem        `json:"subitems"`
}

// ─── Value parsers ──────────────────────────────────────────────────

// parseDateValue reads a date column. `time`, when present, is UTC.
func parseDateValue(cv mondayColumnValue) *time.Time {
	if b := cv.valueJSON(); b != nil {
		var v struct {
			Date string `json:"date"`
			Time string `json:"time"`
		}
		if json.Unmarshal(b, &v) == nil && v.Date != "" {
			if v.Time != "" {
				if t, err := time.Parse("2006-01-02 15:04:05", v.Date+" "+v.Time); err == nil {
					return &t
				}
			}
			if t := parseDay(v.Date); t != nil {
				return t
			}
		}
	}
	// Fallback: text is "YYYY-MM-DD" or "YYYY-MM-DD HH:MM" in the
	// account's zone; the day is what matters for a due date.
	return parseDay(firstField(cv.text()))
}

// parseTimelineValue reads a timeline column: one column, both ends.
func parseTimelineValue(cv mondayColumnValue) (from, to *time.Time) {
	if b := cv.valueJSON(); b != nil {
		var v struct {
			From string `json:"from"`
			To   string `json:"to"`
		}
		if json.Unmarshal(b, &v) == nil && (v.From != "" || v.To != "") {
			return parseDay(v.From), parseDay(v.To)
		}
	}
	txt := cv.text()
	if txt == "" {
		return nil, nil
	}
	// "2026-10-01 - 2026-10-10". Split on " - " so the dashes inside
	// the dates are left alone.
	parts := strings.SplitN(txt, " - ", 2)
	from = parseDay(strings.TrimSpace(parts[0]))
	if len(parts) == 2 {
		to = parseDay(strings.TrimSpace(parts[1]))
	}
	return from, to
}

// parsePeopleValue returns the person ids in a people column. Teams
// (kind "team") are skipped: they aren't users and can't be assignees.
func parsePeopleValue(cv mondayColumnValue) []string {
	b := cv.valueJSON()
	if b == nil {
		return nil
	}
	var v struct {
		PersonsAndTeams []struct {
			ID   json.RawMessage `json:"id"`
			Kind string          `json:"kind"`
		} `json:"personsAndTeams"`
	}
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	out := make([]string, 0, len(v.PersonsAndTeams))
	seen := map[string]bool{}
	for _, pt := range v.PersonsAndTeams {
		if pt.Kind != "" && !strings.EqualFold(pt.Kind, "person") {
			continue
		}
		id := rawID(pt.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// parseTagsValue returns the tag names. `value` only carries tag ids,
// so the names come from `text` ("bug, ui").
func parseTagsValue(cv mondayColumnValue) []string {
	return splitList(cv.text())
}

// parseLabel returns a status / dropdown / priority label, with the
// decorative emoji monday ships in its defaults ("Critical ⚠️") removed
// so the operator's mapping keys stay typeable.
func parseLabel(cv mondayColumnValue) string {
	return cleanLabel(cv.text())
}

func cleanLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.Is(unicode.So, r), unicode.Is(unicode.Sk, r),
			unicode.Is(unicode.Cf, r), unicode.Is(unicode.Mn, r),
			unicode.Is(unicode.Cs, r), r == '️', r == '︎':
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

func rawID(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

func parseDay(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return &t
	}
	return nil
}

func parseStamp(s *string) time.Time {
	if s == nil || *s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05 MST", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, *s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func firstField(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ─── Column picks ───────────────────────────────────────────────────

func titleHas(cv mondayColumnValue, words ...string) bool {
	t := strings.ToLower(cv.title())
	for _, w := range words {
		if strings.Contains(t, w) {
			return true
		}
	}
	return false
}

func isPriorityColumn(cv mondayColumnValue) bool {
	k := cv.kind()
	return (k == "status" || k == "dropdown" || k == "priority") && titleHas(cv, "priority", "urgency")
}

// columnPicks is which column_value feeds which task field.
type columnPicks struct {
	status, priority   *mondayColumnValue
	people             *mondayColumnValue
	timeline           *mondayColumnValue
	startDate, dueDate *mondayColumnValue
	otherDate          *mondayColumnValue
	tags               []*mondayColumnValue
	used               map[string]bool
}

func pickColumns(cvs []mondayColumnValue) columnPicks {
	pk := columnPicks{used: map[string]bool{}}
	var firstStatus, firstPeople *mondayColumnValue
	for i := range cvs {
		cv := &cvs[i]
		switch cv.kind() {
		case "status", "dropdown", "priority":
			if isPriorityColumn(*cv) {
				if pk.priority == nil {
					pk.priority = cv
				}
				continue
			}
			if cv.kind() == "dropdown" {
				if titleHas(*cv, "tag", "label") {
					pk.tags = append(pk.tags, cv)
				}
				continue
			}
			if pk.status == nil && strings.EqualFold(strings.TrimSpace(cv.title()), "status") {
				pk.status = cv
			}
			if firstStatus == nil {
				firstStatus = cv
			}
		case "people":
			if pk.people == nil && titleHas(*cv, "owner", "assign", "person", "people", "responsible", "dri") {
				pk.people = cv
			}
			if firstPeople == nil {
				firstPeople = cv
			}
		case "timeline":
			if pk.timeline == nil {
				pk.timeline = cv
			}
		case "date":
			switch {
			case titleHas(*cv, "start", "begin", "kick"):
				if pk.startDate == nil {
					pk.startDate = cv
				}
			case titleHas(*cv, "due", "deadline", "end", "eta", "target"):
				if pk.dueDate == nil {
					pk.dueDate = cv
				}
			default:
				if pk.otherDate == nil {
					pk.otherDate = cv
				}
			}
		case "tags":
			pk.tags = append(pk.tags, cv)
		}
	}
	if pk.status == nil {
		pk.status = firstStatus
	}
	if pk.people == nil {
		pk.people = firstPeople
	}
	for _, cv := range []*mondayColumnValue{pk.status, pk.priority, pk.people, pk.timeline, pk.startDate, pk.dueDate, pk.otherDate} {
		if cv != nil {
			pk.used[cv.ID] = true
		}
	}
	for _, cv := range pk.tags {
		pk.used[cv.ID] = true
	}
	return pk
}

// Column types never rendered into the description: structural, empty
// by nature, or already imported elsewhere (files → attachments).
var skipInDescription = map[string]bool{
	"name": true, "subtasks": true, "subitems": true, "file": true,
	"button": true, "doc": true, "direct_doc": true, "creation_log": true,
	"last_updated": true, "item_id": true, "group": true,
}

// ─── Item → SourceTask ──────────────────────────────────────────────

// itemReading is the task-shaped reading of one item, before it is put
// into a SourceTask. Split out so tests can assert it directly.
type itemReading struct {
	Status    string
	Priority  string
	Assignees []string
	Labels    []string
	Start     *time.Time
	Due       *time.Time
	Extra     [][2]string // title, text of columns not mapped above
}

func readItemFields(item mondayItem) itemReading {
	pk := pickColumns(item.ColumnValues)
	var f itemReading
	if pk.status != nil {
		f.Status = parseLabel(*pk.status)
	}
	if f.Status == "" && item.Group != nil {
		// No status column (or an unset one): the group is how many
		// boards express workflow ("To do", "Doing", "Done").
		f.Status = cleanLabel(item.Group.Title)
	}
	if pk.priority != nil {
		f.Priority = parseLabel(*pk.priority)
	}
	if pk.people != nil {
		f.Assignees = parsePeopleValue(*pk.people)
	}

	var tlFrom, tlTo *time.Time
	if pk.timeline != nil {
		tlFrom, tlTo = parseTimelineValue(*pk.timeline)
	}
	if pk.startDate != nil {
		f.Start = parseDateValue(*pk.startDate)
	}
	if f.Start == nil {
		f.Start = tlFrom
	}
	if pk.dueDate != nil {
		f.Due = parseDateValue(*pk.dueDate)
	}
	if f.Due == nil {
		f.Due = tlTo
	}
	if f.Due == nil && pk.otherDate != nil {
		f.Due = parseDateValue(*pk.otherDate)
	} else if pk.otherDate != nil {
		pk.used[pk.otherDate.ID] = false // keep it visible in the description
	}

	seen := map[string]bool{}
	for _, cv := range pk.tags {
		for _, t := range parseTagsValue(*cv) {
			if k := strings.ToLower(t); !seen[k] {
				seen[k] = true
				f.Labels = append(f.Labels, t)
			}
		}
	}

	for _, cv := range item.ColumnValues {
		if pk.used[cv.ID] || skipInDescription[cv.kind()] || skipInDescription[cv.ID] {
			continue
		}
		if txt := cv.text(); txt != "" {
			f.Extra = append(f.Extra, [2]string{cv.title(), txt})
		}
	}
	return f
}

// renderExtra turns the unmapped columns into a small definition list
// so the imported task keeps everything the board showed.
func renderExtra(extra [][2]string) string {
	if len(extra) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<p><strong>monday.com fields</strong></p><ul>")
	for _, kv := range extra {
		b.WriteString("<li><strong>")
		b.WriteString(html.EscapeString(kv[0]))
		b.WriteString(":</strong> ")
		b.WriteString(html.EscapeString(helpers.TruncateRunes(kv[1], 2000)))
		b.WriteString("</li>")
	}
	b.WriteString("</ul>")
	return b.String()
}

// itemToSourceTask maps one item (or subitem when parentID != "").
// commentAssetIDs are assets that belong to an update; they are
// imported on the comment, so they're dropped from the task.
func (p *Provider) itemToSourceTask(item mondayItem, boardID, parentID string, commentCount int, commentAssetIDs map[string]bool) importProvider.SourceTask {
	f := readItemFields(item)

	atts := make([]importProvider.SourceAttachment, 0, len(item.Assets))
	for _, a := range item.Assets {
		if a.ID == "" || commentAssetIDs[a.ID] {
			continue
		}
		atts = append(atts, assetRef(a, "task", item.ID))
	}

	group := ""
	if item.Group != nil {
		group = item.Group.Title
	}
	columns := make(map[string]string, len(item.ColumnValues))
	for _, cv := range item.ColumnValues {
		if txt := cv.text(); txt != "" {
			columns[cv.title()] = helpers.TruncateRunes(txt, 2000)
		}
	}

	return importProvider.SourceTask{
		SourceID:        item.ID,
		ParentTaskID:    parentID,
		ProjectSourceID: boardID,
		Name:            helpers.TruncateRunes(strings.TrimSpace(item.Name), 256),
		Description:     renderExtra(f.Extra),
		Status:          f.Status,
		Priority:        f.Priority,
		Labels:          f.Labels,
		AssigneeIds:     f.Assignees,
		CreatedBy:       deref(item.CreatorID),
		StartDate:       f.Start,
		DueDate:         f.Due,
		Created:         parseStamp(item.CreatedAt),
		Updated:         parseStamp(item.UpdatedAt),
		Completed:       importProvider.ApplyStatusMap(f.Status, nil, p.DefaultStatusMap()) == "done",
		AttachmentRefs:  atts,
		CommentCount:    commentCount,
		SubtaskCount:    len(item.Subitems),
		Metadata: map[string]any{
			"monday_url":      item.URL,
			"monday_item_id":  item.ID,
			"monday_board_id": boardID,
			"monday_group":    group,
			"monday_state":    item.State,
			"monday_columns":  columns,
		},
	}
}

func assetRef(a mondayAsset, kind, parentID string) importProvider.SourceAttachment {
	name := strings.TrimSpace(a.Name)
	if name == "" {
		name = "attachment"
		if a.FileExtension != "" {
			name += "." + strings.TrimPrefix(a.FileExtension, ".")
		}
	}
	return importProvider.SourceAttachment{
		SourceID: a.ID,
		Name:     name,
		// public_url is a presigned S3 link that expires within the
		// hour; FetchAttachment re-resolves it by asset id at download
		// time. Kept here only as a fallback.
		URL:    a.PublicURL,
		Size:   a.FileSize,
		Mime:   helpers.GuessContentTypeFromName(name),
		Parent: importProvider.SourceRef{Kind: kind, SourceID: parentID},
	}
}

// updatesToComments flattens updates and their replies into
// chronological comments. Bodies are monday's HTML, sanitised.
func updatesToComments(itemID string, updates []mondayUpdate) []importProvider.SourceComment {
	out := make([]importProvider.SourceComment, 0, len(updates))
	for _, u := range updates {
		c := importProvider.SourceComment{
			SourceID:       u.ID,
			TaskSourceID:   itemID,
			Body:           commentHTML(u.Body, u.TextBody),
			AuthorSourceID: deref(u.CreatorID),
			Created:        parseStamp(u.CreatedAt),
		}
		for _, a := range u.Assets {
			if a.ID != "" {
				c.AttachmentRefs = append(c.AttachmentRefs, assetRef(a, "comment", u.ID))
			}
		}
		out = append(out, c)
		for _, r := range u.Replies {
			if r.ID == "" {
				continue
			}
			out = append(out, importProvider.SourceComment{
				SourceID:       r.ID, // replies are updates too; ids are global
				TaskSourceID:   itemID,
				Body:           commentHTML(r.Body, r.TextBody),
				AuthorSourceID: deref(r.CreatorID),
				Created:        parseStamp(r.CreatedAt),
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

func commentHTML(body, text string) string {
	if strings.TrimSpace(body) != "" {
		return strings.TrimSpace(htmlPolicy.Sanitize(body))
	}
	return helpers.PlainTextToHTML(text)
}
