package business

// OneCamp's own doc templates: the ten every workspace starts with, in the
// order the gallery and "New doc" show them. Their ids are fixed (the web asks
// for them by id), and their bodies are the editor's own HTML: headings,
// paragraphs, lists and to-dos. A template says what goes where and leaves
// the writing to the person: no filler to delete first.

import "github.com/google/uuid"

// BuiltinDocTemplate is one of OneCamp's own doc templates.
type BuiltinDocTemplate struct {
	ID          uuid.UUID
	Name        string
	Description string
	Hue         string
	Glyph       string
	Category    string
	Body        string
}

// todo is an open to-do holding text (or nothing).
func todo(text string) string {
	return `<li data-type="taskItem" data-checked="false"><p>` + text + `</p></li>`
}

// todos is a to-do list.
func todos(items ...string) string {
	out := `<ul data-type="taskList">`
	for _, t := range items {
		out += todo(t)
	}
	return out + `</ul>`
}

// bullets is a bulleted list.
func bullets(items ...string) string {
	out := `<ul>`
	for _, t := range items {
		out += `<li><p>` + t + `</p></li>`
	}
	return out + `</ul>`
}

// field is a labelled line to fill in ("Owner: ").
func field(label string) string {
	return `<p><strong>` + label + `:</strong> </p>`
}

// BuiltinDocTemplates are OneCamp's own doc templates, in order.
var BuiltinDocTemplates = []BuiltinDocTemplate{
	{
		ID:          uuid.MustParse("b5d0c000-0000-4000-8000-000000000001"),
		Name:        "Meeting notes",
		Description: "Who came, what was decided, and who does what next.",
		Hue:         "sky",
		Glyph:       "list-todo",
		Category:    "Meetings",
		Body: field("Date") + field("Attendees") +
			`<h2>Agenda</h2>` + bullets("") +
			`<h2>Notes</h2><p></p>` +
			`<h2>Decisions</h2>` + bullets("") +
			`<h2>Action items</h2>` + todos(""),
	},
	{
		ID:          uuid.MustParse("b5d0c000-0000-4000-8000-000000000002"),
		Name:        "Project brief",
		Description: "Why it matters, what ships, who owns it and when.",
		Hue:         "sun",
		Glyph:       "file-text",
		Category:    "Planning",
		Body: `<h2>Why</h2><p></p>` +
			`<h2>What we will ship</h2>` + bullets("") +
			`<h2>What we won't do</h2>` + bullets("") +
			`<h2>Who</h2>` + field("Owner") + field("Team") +
			`<h2>When</h2>` + todos("Kick-off", "First version in people's hands", "Launch") +
			`<h2>How we'll know it worked</h2>` + bullets("") +
			`<h2>Risks</h2>` + bullets(""),
	},
	{
		ID:          uuid.MustParse("b5d0c000-0000-4000-8000-000000000003"),
		Name:        "Decision record",
		Description: "The options, the call, and what follows from it.",
		Hue:         "dusk",
		Glyph:       "check-square",
		Category:    "Decisions",
		Body: `<p><strong>Status:</strong> Proposed</p>` + field("Decider") + field("Decide by") +
			`<h2>Context</h2><p></p>` +
			`<h2>Options</h2><ol><li><p></p></li><li><p></p></li></ol>` +
			`<h2>Decision</h2><p></p>` +
			`<h2>Consequences</h2><p></p>`,
	},
	{
		ID:          uuid.MustParse("b5d0c000-0000-4000-8000-000000000004"),
		Name:        "Weekly update",
		Description: "What moved this week, what's next and what's stuck.",
		Hue:         "moss",
		Glyph:       "calendar",
		Category:    "Updates",
		Body: `<h2>Highlights</h2>` + bullets("") +
			`<h2>Done this week</h2>` + bullets("") +
			`<h2>Next week</h2>` + todos("") +
			`<h2>Blocked or at risk</h2>` + bullets("") +
			`<h2>Numbers</h2><p></p>`,
	},
	{
		ID:          uuid.MustParse("b5d0c000-0000-4000-8000-000000000005"),
		Name:        "RFC",
		Description: "A proposal for a change that's hard to undo, open for review.",
		Hue:         "lake",
		Glyph:       "lightbulb",
		Category:    "Engineering",
		Body: field("Author") + field("Reviewers") + `<p><strong>Status:</strong> Draft</p>` +
			`<h2>Summary</h2><p></p>` +
			`<h2>Problem</h2><p></p>` +
			`<h2>Proposal</h2><p></p>` +
			`<h2>Alternatives considered</h2>` + bullets("") +
			`<h2>Rollout and rollback</h2><p></p>` +
			`<h2>Open questions</h2>` + bullets(""),
	},
	{
		ID:          uuid.MustParse("b5d0c000-0000-4000-8000-000000000006"),
		Name:        "Retrospective",
		Description: "What went well, what didn't, and what to try next.",
		Hue:         "berry",
		Glyph:       "rotate-ccw",
		Category:    "Meetings",
		Body: field("Period") + field("People") +
			`<h2>What went well</h2>` + bullets("") +
			`<h2>What didn't</h2>` + bullets("") +
			`<h2>What we'll try next</h2>` + todos("") +
			`<h2>Thanks</h2><p></p>`,
	},
	{
		ID:          uuid.MustParse("b5d0c000-0000-4000-8000-000000000007"),
		Name:        "One-to-one",
		Description: "A running page for two people who meet regularly.",
		Hue:         "lake",
		Glyph:       "users",
		Category:    "Meetings",
		Body: field("With") + field("Date") +
			`<h2>How things are going</h2><p></p>` +
			`<h2>Topics</h2>` + bullets("") +
			`<h2>Feedback, both ways</h2><p></p>` +
			`<h2>Follow-ups</h2>` + todos(""),
	},
	{
		ID:          uuid.MustParse("b5d0c000-0000-4000-8000-000000000008"),
		Name:        "Incident review",
		Description: "What happened, when, and what changes now. Blameless.",
		Hue:         "berry",
		Glyph:       "alert-triangle",
		Category:    "Engineering",
		Body: field("Severity") + field("Started") + field("Resolved") + field("Owner") +
			`<blockquote><p>We look at how the system let this happen, not at who.</p></blockquote>` +
			`<h2>Summary</h2><p></p>` +
			`<h2>Impact</h2><p></p>` +
			`<h2>Timeline</h2>` + bullets("") +
			`<h2>What went wrong</h2><p></p>` +
			`<h2>What went right</h2><p></p>` +
			`<h2>Follow-up actions</h2>` + todos(""),
	},
	{
		ID:          uuid.MustParse("b5d0c000-0000-4000-8000-000000000009"),
		Name:        "Onboarding guide",
		Description: "A new teammate's first two weeks, as a checklist.",
		Hue:         "moss",
		Glyph:       "rocket",
		Category:    "Teams",
		Body: `<p>Welcome. This page is everything you need for your first two weeks.</p>` +
			`<h2>Your first day</h2>` + todos("Set your photo and working hours", "Join the team's channels", "Meet your onboarding buddy") +
			`<h2>Your first week</h2>` + todos("Read how we work", "Pair on a small task", "Ship something small") +
			`<h2>People to know</h2>` + bullets("") +
			`<h2>Where things live</h2>` + bullets(""),
	},
	{
		ID:          uuid.MustParse("b5d0c000-0000-4000-8000-00000000000a"),
		Name:        "Release notes",
		Description: "What's new, better and fixed, for the people who use it.",
		Hue:         "sun",
		Glyph:       "megaphone",
		Category:    "Updates",
		Body: field("Released") +
			`<h2>New</h2>` + bullets("") +
			`<h2>Improved</h2>` + bullets("") +
			`<h2>Fixed</h2>` + bullets("") +
			`<h2>Thanks</h2><p></p>`,
	},
}
