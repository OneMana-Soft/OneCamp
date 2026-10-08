package business

import (
	taskFieldBusiness "github.com/akashc777/OneCamp/business/TaskField"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// The built-in templates, for the work small teams and agencies start most
// often. Each task says what "done" looks like, so a new project reads as a
// plan, not a list of headings. Dates are days from the start; tasks are
// unassigned, and nothing here names a person.

const (
	low    = dgraphStruct.TASK_PRIORITY_LOW
	medium = dgraphStruct.TASK_PRIORITY_MEDIUM
	high   = dgraphStruct.TASK_PRIORITY_HIGH

	backlog = dgraphStruct.TASK_STATUS_BACKLOG
)

func on(day int) *int { return &day }

// text is a description written as plain text ("- " starts a list item),
// kept as the editor's HTML.
func text(s string) string { return helpers.PlainTextToHTML(s) }

// steps are subtasks without dates.
func steps(names ...string) []Subtask {
	out := make([]Subtask, len(names))
	for i, n := range names {
		out[i] = Subtask{Name: n}
	}
	return out
}

var builtins = []Template{
	{
		ID:          "client-project",
		Name:        "Client project",
		Description: "Kickoff to invoice for one client: assets, drafts, their review, delivery and a testimonial.",
		Statuses:    []Status{{Name: "Client review", Category: dgraphStruct.TASK_STATUS_INREVIEW, Color: "amber"}},
		Fields:      []Field{{Name: "Client approved", Type: "checkbox", OnCard: true}},
		Tasks: []Task{
			{Name: "Kickoff call", Priority: high, Tags: "client", DueDay: on(1), Description: text("Agree on the goal, the scope, the deadline and who signs off. Put the notes in this task so everyone works from the same page.")},
			{Name: "Collect brand assets and access", Priority: high, Tags: "client", DueDay: on(3), Description: text("Ask for everything in one message, so the client answers once."),
				Subtasks: steps("Logo files and fonts", "Brand colours", "Copy and images", "Logins we'll need")},
			{Name: "Share the timeline with the client", Priority: medium, Tags: "client", DueDay: on(4), Description: text("The dates they'll see work, and the dates we need their answers by. Share the project with them (Share with a client) so they can follow along.")},
			{Name: "First draft", Priority: high, StartDay: on(4), DueDay: on(10), Description: text("Good enough to react to, not polished. Note any open questions at the top.")},
			{Name: "Send the draft for review", Priority: medium, Tags: "client", DueDay: on(11), Description: text("Move the task to Client review. Ask for all feedback in one round, by a date.")},
			{Name: "Revisions", Priority: medium, StartDay: on(12), DueDay: on(15), Description: text("Work through the feedback. Anything outside the agreed scope gets its own task and its own price.")},
			{Name: "Final delivery", Priority: high, Tags: "client", DueDay: on(17), Description: text("Hand over the files and the logins, with a short note on how to use what we made.")},
			{Name: "Send the invoice", Priority: medium, DueDay: on(18), Description: text("If the team logged time on this project, OneCamp makes the invoice from it: open the project's time (the clock) and choose Make an invoice.")},
			{Name: "Ask for a testimonial", Priority: low, Tags: "client", DueDay: on(25), Description: text("A week after delivery, when the work has proven itself. Two or three sentences in their words is plenty.")},
		},
	},
	{
		ID:          "product-launch",
		Name:        "Product launch",
		Description: "Two weeks to launch day: positioning, the page, the post, the emails and a retro.",
		Fields: []Field{{Name: "Channel", Type: "select", OnCard: true, Options: []taskFieldBusiness.OptionInput{
			{Label: "Blog", Color: "violet"}, {Label: "Email", Color: "sky"}, {Label: "Social", Color: "pink"}, {Label: "Press", Color: "amber"},
		}}},
		Tasks: []Task{
			{Name: "Write the positioning brief", Priority: high, Tags: "launch", DueDay: on(2), Description: text("Who it's for, the problem it solves, why now, and the one sentence everyone will repeat.")},
			{Name: "Draft the launch page", Priority: high, Tags: "launch", StartDay: on(2), DueDay: on(7), Description: text("Lead with the problem and the one sentence from the brief. One clear call to action.")},
			{Name: "Check pricing and plans", Priority: medium, DueDay: on(7), Description: text("The numbers on the page, the checkout and the docs all say the same thing.")},
			{Name: "Record a short demo", Priority: medium, Tags: "launch", DueDay: on(10), Description: text("Two minutes or less, one take is fine. Show the problem, then the product solving it.")},
			{Name: "Write the announcement post", Priority: medium, Tags: "launch", DueDay: on(10), Description: text("What's new, who it helps and how to try it. Link the demo.")},
			{Name: "Email existing customers", Priority: medium, Tags: "launch", DueDay: on(13), Description: text("Short and personal: what changed for them and the one thing to try first.")},
			{Name: "Launch day", Priority: high, Tags: "launch", DueDay: on(14), Description: text("Keep this task open all day and tick the steps off as they go out."),
				Subtasks: steps("Publish the page and the post", "Send the emails", "Share it where our people are", "Answer every question within the hour")},
			{Name: "Launch retro", Priority: low, DueDay: on(21), Description: text("Thirty minutes: the numbers, what worked, and what we'd do differently next time.")},
		},
	},
	{
		ID:          "website-redesign",
		Name:        "Website redesign",
		Description: "From an audit of the old site to launch, with redirects so links and search rankings carry over.",
		Statuses:    []Status{{Name: "Design review", Category: dgraphStruct.TASK_STATUS_INREVIEW, Color: "violet"}},
		Tasks: []Task{
			{Name: "Audit the current site", Priority: high, DueDay: on(3), Description: text("Which pages people visit, what's out of date and what's broken. Export the analytics before anything changes.")},
			{Name: "Sitemap and content plan", Priority: high, DueDay: on(7), Description: text("Every page the new site will have, what it's for, and who writes it.")},
			{Name: "Wireframes", Priority: medium, StartDay: on(7), DueDay: on(12), Description: text("Layout only, in grey. Agree on these before anything gets colour.")},
			{Name: "Visual design", Priority: high, StartDay: on(12), DueDay: on(19), Description: text("The home page and one inner page first; the rest follow their lead.")},
			{Name: "Write and collect the copy", Priority: medium, DueDay: on(19), Description: text("Real words, not placeholder text, before the build starts.")},
			{Name: "Build", Priority: high, StartDay: on(19), DueDay: on(33), Description: text("Every page in the sitemap built from the visual design and the real copy, on a staging address the client can see.")},
			{Name: "Test on phones and browsers", Priority: high, DueDay: on(36), Description: text("Every page and form on each of these. Each problem found becomes its own task."),
				Subtasks: steps("iPhone Safari", "Android Chrome", "Desktop Chrome, Firefox and Safari", "Forms send and arrive", "Page speed")},
			{Name: "Set up redirects", Priority: high, DueDay: on(37), Description: text("Map every old address to its new one, so old links and search rankings carry over.")},
			{Name: "Launch", Priority: high, DueDay: on(38), Description: text("The domain points at the new site, the redirects are on, and the home page and a form work from outside the office.")},
			{Name: "Check analytics and search a week on", Priority: low, DueDay: on(45), Description: text("Look for pages that lost visits and addresses that now lead nowhere.")},
		},
	},
	{
		ID:          "feature-build",
		Name:        "Feature build",
		Description: "One feature from spec to release: design, build, code review, QA and the release note.",
		Statuses:    []Status{{Name: "QA", Category: dgraphStruct.TASK_STATUS_INREVIEW, Color: "violet"}},
		Tasks: []Task{
			{Name: "Write the spec", Priority: high, DueDay: on(2), Description: text("The problem, who has it, what done looks like, and what's out of scope.")},
			{Name: "Design", Priority: medium, StartDay: on(2), DueDay: on(5), Description: text("The flows and the screens, the empty and error states included.")},
			{Name: "Build", Priority: high, StartDay: on(5), DueDay: on(12), Description: text("Built to the spec, behind a flag if it's risky, and working end to end on a staging server.")},
			{Name: "Tests", Priority: medium, DueDay: on(12), Description: text("The cases from the spec, and the bug you'd be embarrassed to ship.")},
			{Name: "Code review", Priority: medium, DueDay: on(13), Description: text("Someone who didn't write it reads it, runs it and approves it. What they find is fixed before QA.")},
			{Name: "QA pass", Priority: high, DueDay: on(14), Description: text("Move tasks to QA as they're ready. Test it as the person in the spec would use it.")},
			{Name: "Release", Priority: high, DueDay: on(15), Description: text("Shipped, with someone watching the errors and the first people using it for an hour after.")},
			{Name: "Write the release note", Priority: low, DueDay: on(15), Description: text("What changed and who it helps, in a sentence or two.")},
		},
	},
	{
		ID:          "event",
		Name:        "Event",
		Description: "Six weeks to a meetup, an offsite or a conference day, and the follow-up after it.",
		Tasks: []Task{
			{Name: "Set the date, the budget and the goal", Priority: high, DueDay: on(1), Description: text("The date, what there is to spend, and what a good event looks like: how many people, and what they leave with.")},
			{Name: "Book the venue", Priority: high, DueDay: on(7), Description: text("Check the capacity, the access, the Wi-Fi and what's included before signing.")},
			{Name: "Plan the agenda and invite speakers", Priority: medium, DueDay: on(14), Description: text("The day's timings drafted, and each speaker confirmed in writing with their topic and slot.")},
			{Name: "Open registration", Priority: medium, DueDay: on(14), Description: text("A sign-up form with the date, the place and a cap on numbers, shared where your people are.")},
			{Name: "Order food and drinks", Priority: medium, DueDay: on(30), Description: text("Ask about dietary needs in the registration form.")},
			{Name: "Badges, signs and the run sheet", Priority: low, DueDay: on(38), Description: text("Everything needed on the day, printed and in one box."),
				Subtasks: steps("Print badges", "Signs to the room", "Run sheet: who does what, minute by minute")},
			{Name: "Remind the attendees", Priority: medium, DueDay: on(40), Description: text("Where, when, how to get there and who to call on the day.")},
			{Name: "Event day", Priority: high, DueDay: on(42), Description: text("Keep this task open on the day for the run sheet, who to call, and anything that changes.")},
			{Name: "Send thanks, photos and slides", Priority: medium, DueDay: on(44), Description: text("Within two days, while people still remember it: thanks, photos, slides and anything you promised to send.")},
			{Name: "Event retro", Priority: low, DueDay: on(49), Description: text("Thirty minutes on the numbers, what worked and what to change next time, written down in this task.")},
		},
	},
	{
		ID:          "new-hire-onboarding",
		Name:        "New hire onboarding",
		Description: "From a week before their first day to the 90-day review: accounts, a buddy and check-ins.",
		Tasks: []Task{
			{Name: "Send the welcome email", Priority: high, Tags: "onboarding", DueDay: on(0), Description: text("The start date and time, where to go, who to ask for, and what to bring.")},
			{Name: "Set up their accounts", Priority: high, Tags: "onboarding", DueDay: on(3), Description: text("Every account ready before their first morning, so day one isn't spent waiting for access."),
				Subtasks: steps("Email", "Invite to OneCamp and their team's channels", "The tools for their role", "Payroll and documents")},
			{Name: "Get their laptop ready", Priority: high, Tags: "onboarding", DueDay: on(4), Description: text("Set up, updated and signed in to the basics, with the password manager and two-factor sign-in ready.")},
			{Name: "Pick a buddy", Priority: medium, Tags: "onboarding", DueDay: on(5), Description: text("Someone outside their reporting line to ask the questions they won't ask their manager.")},
			{Name: "First day: welcome, tour and lunch", Priority: high, Tags: "onboarding", DueDay: on(7), Description: text("They meet the team, see where things are, and have lunch with their buddy. Nothing urgent on day one.")},
			{Name: "Walk through how we work", Priority: medium, Tags: "onboarding", DueDay: on(8), Description: text("The handbook, the policies, and where things live in OneCamp.")},
			{Name: "Their first small task, shipped", Priority: medium, Tags: "onboarding", DueDay: on(11), Description: text("Something real and small, done in their first week.")},
			{Name: "Two-week check-in", Priority: medium, Tags: "onboarding", DueDay: on(21), Description: text("Thirty minutes with their manager: what's clear, what isn't, and what they need.")},
			{Name: "30-day check-in", Priority: medium, Tags: "onboarding", DueDay: on(37), Description: text("What they've done, what's slowing them down, and their goals for the next two months.")},
			{Name: "90-day review", Priority: medium, Tags: "onboarding", DueDay: on(97), Description: text("How the first three months went against the goals set at 30 days, and what comes next.")},
		},
	},
	{
		ID:          "security-audit-prep",
		Name:        "Security audit prep",
		Description: "The groundwork before an auditor or a customer's security review: policies, access, backups and evidence.",
		Tasks: []Task{
			{Name: "Agree on the scope and the auditor", Priority: high, Tags: "audit", DueDay: on(3), Description: text("Which standard or questionnaire, which systems are in scope, and the dates.")},
			{Name: "Gather the policies", Priority: high, Tags: "audit", DueDay: on(10), Description: text("Keep each policy as a doc, with its owner and the date it was last reviewed."),
				Subtasks: steps("Information security", "Access control", "Incident response", "Backups and recovery", "Vendor management")},
			{Name: "Review who has access to what", Priority: high, Tags: "audit", DueDay: on(14), Description: text("Remove the accounts of people who left, and admin rights nobody uses.")},
			{Name: "Review vendors", Priority: medium, Tags: "audit", DueDay: on(17), Description: text("Every service that holds our data or our customers' data, and what it holds.")},
			{Name: "Test a restore from backup", Priority: high, Tags: "audit", DueDay: on(21), Description: text("Restore to a spare server, check the data is all there, and note how long it took.")},
			{Name: "Run an incident drill", Priority: medium, Tags: "audit", DueDay: on(24), Description: text("Walk through a made-up incident, such as a lost laptop or a leaked password, using the incident response policy. Note what didn't work.")},
			{Name: "Collect the evidence", Priority: high, Tags: "audit", DueDay: on(28), Description: text("Screenshots, exports and logs, one folder per control, named so the auditor can find them.")},
			{Name: "Walk the auditor through it", Priority: high, Tags: "audit", DueDay: on(35), Description: text("The scope, the policies and where each piece of evidence is. Note every question that couldn't be answered on the spot.")},
			{Name: "Fix what they found", Priority: medium, Tags: "audit", Status: backlog, DueDay: on(49), Description: text("One task per finding, each with an owner and a date.")},
		},
	},
}

// builtIn is the built-in template with that id, or nil.
func builtIn(id string) *Template {
	for i := range builtins {
		if builtins[i].ID == id {
			return &builtins[i]
		}
	}
	return nil
}
