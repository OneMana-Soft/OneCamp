package business

// What a new workspace still needs, derived from the workspace itself.
//
// WHY IT EXISTS. Setup ended at "create an admin account" and redirected to the
// dashboard. A buyer who had just stood up a fifteen-service stack landed in an
// empty room: no channel, no teammates, no prompt, no next step. Everything the
// product does — boards, tables, search across their own connected systems,
// agents, calendar — was reachable only by someone who already knew it was there.
// The breadth is the reason for the price, and an empty workspace communicates
// the opposite of breadth.
//
// DERIVED, NOT STORED. Each step is a question asked of the live workspace rather
// than a checkbox somebody has to remember to tick. A stored "invited teammates:
// true" is wrong the moment the last teammate is removed, and it is wrong
// silently. The only thing persisted is whether the admin dismissed the list,
// because that is a preference and cannot be derived from anything.
//
// NO AI IMPORTS. This package is compiled into BOTH editions, and the AI-free
// edition does not contain the AI packages at all. So the AI steps are not
// written here: the AI packages register them through Register from their own
// init, and on the edition that does not link those packages the steps do not
// exist. This is the same inversion helpers/features.go was built for: the
// subsystem announces itself, the caller never imports it.

import (
	"context"
	"fmt"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	"sort"
	"strings"
	"sync"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	importDomain "github.com/akashc777/OneCamp/domain/Import"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	configModels "github.com/akashc777/OneCamp/models/postgres/Config"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	emailService "github.com/akashc777/OneCamp/services/Email"
)

// dismissedKey is the workspace-level flag in system_configs. Workspace-scoped
// rather than per-user on purpose: the list describes the state of the WORKSPACE,
// so a second admin should not be shown a setup list the first admin already
// worked through.
const dismissedKey = "onboarding_dismissed"

// skippedKey holds every set-aside step id in ONE workspace-level row, comma
// separated. One row rather than one per step, for the same reason the sidebar
// keeps its section states in one cookie: the number of steps is small, they are
// read together on every dashboard load, and a row per step turns one read into
// as many reads as there are steps.
//
// Workspace-scoped like the dismissal above: the list describes the workspace, so
// a second admin should not be shown a step the first one has already decided
// does not apply here.
const skippedKey = "onboarding_skipped"

// Step is one thing a new workspace still needs, and where to go and do it.
type Step struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	// Href is a frontend route. Kept here rather than in the client so the step
	// and its destination cannot drift apart in a later edit to either side.
	Href string `json:"href"`
	Done bool   `json:"done"`

	// Skippable marks a step that does not apply to every workspace, so it can
	// be set aside instead of sitting unfinished forever.
	//
	// WHY ANY STEP NEEDS THIS. "Bring your Slack history over" is the right first
	// thing for a team migrating and meaningless for one that is not, and there
	// is no signal anywhere that says which this is. Without a way to set a step
	// aside, such a step is either missing from the list for the people who most
	// need it, or permanently unfinished for everyone else. "Set up email" had
	// the milder version of the same problem: an operator running deliberately
	// without mail was nagged for the life of the workspace.
	Skippable bool `json:"skippable,omitempty"`
	// Skipped reports that this admin set it aside. Still returned rather than
	// filtered out, because a list you can hide things from has to be a list you
	// can get them back from.
	Skipped bool `json:"skipped,omitempty"`

	// SelfHostedOnly marks a step OneCamp Cloud does for a workspace it runs
	// (settings Managed): email, the model provider. There it is not on the
	// list at all, rather than telling an admin to do what is done for them.
	SelfHostedOnly bool `json:"-"`
}

// State is the whole answer: what is left, and whether to show it at all.
type State struct {
	Dismissed bool   `json:"dismissed"`
	Steps     []Step `json:"steps"`
	Done      int    `json:"done"`
	Total     int    `json:"total"`
	// Complete is Done == Total, computed here so three callers do not each
	// re-derive it and disagree about whether an empty list counts as complete.
	//
	// A SKIPPED STEP IS NOT COUNTED IN EITHER, so it cannot hold Complete open
	// and cannot be claimed as an achievement. Total is the work still on the
	// list; Skipped is what was set aside.
	Complete bool `json:"complete"`
	Skipped  int  `json:"skipped"`
}

// stepDef is a step and the question that decides whether it is finished.
//
// A slice of definitions rather than a hand-written sequence of if-statements, so
// adding a step is one entry and the evaluation loop never changes. `include`
// decides whether the step applies to this build at all, which is how the AI step
// disappears on the edition that has no AI.
type stepDef struct {
	Step
	include func(features map[string]bool) bool
	// applies decides whether the step is on this admin's list at all; nil
	// means always. For a step that is about one account rather than the
	// workspace, and that has nothing to say once it is dealt with.
	applies func(ctx context.Context, user userModels.UserInfo) bool
	done    func(ctx context.Context, user userModels.UserInfo) bool
}

// Steps an optional subsystem contributes.
//
// The same inversion as the feature registry, for the same reason: this package
// cannot import the AI packages without dragging them into the AI-free edition,
// so a step that only makes sense when a subsystem is present is registered BY
// that subsystem from its own init. Not linking the package is what makes the
// step absent, with no edition check written anywhere.
//
// Appended after the built-in steps, ordered by the weight each contributor gave
// its step. The built-ins are the order a person does things in; a contributed
// step is by nature something extra they do once the workspace exists.
//
// A WEIGHT, NOT REGISTRATION ORDER. The first version appended in the order init
// ran, on the belief that Go initialises sibling packages by import path. It
// does not, quite: a package initialises after everything it imports, and the
// drill package turned out to be reachable from the provider package's imports,
// so "watch an agent be refused" registered before "connect a model provider"
// and the list read backwards. Order is content on this list, so it is stated.
var (
	contributedMu sync.RWMutex
	contributed   []contributedDef
)

type contributedDef struct {
	stepDef
	weight int
}

// Register adds a step from another package. weight orders it among the other
// contributed steps, lowest first; include may be nil, meaning the step applies
// to every build that linked the registering package.
//
// Re-registering the same ID replaces the earlier entry. That keeps tests
// hermetic, and it turns a duplicate registration into the newer definition
// rather than into a checklist that shows the step twice.
func Register(step Step, weight int, include func(features map[string]bool) bool,
	done func(ctx context.Context, user userModels.UserInfo) bool) {
	if step.ID == "" || done == nil {
		return
	}
	contributedMu.Lock()
	defer contributedMu.Unlock()
	def := contributedDef{stepDef: stepDef{Step: step, include: include, done: done}, weight: weight}
	for i := range contributed {
		if contributed[i].ID == step.ID {
			contributed[i] = def
			sortContributedLocked()
			return
		}
	}
	contributed = append(contributed, def)
	sortContributedLocked()
}

// sortContributedLocked orders by weight, and keeps registration order for equal
// weights so two contributors that did not state a preference stay predictable.
func sortContributedLocked() {
	sort.SliceStable(contributed, func(i, j int) bool {
		return contributed[i].weight < contributed[j].weight
	})
}

// allDefinitions is the built-in list followed by whatever was contributed.
func allDefinitions() []stepDef {
	contributedMu.RLock()
	defer contributedMu.RUnlock()
	all := definitions()
	for _, c := range contributed {
		all = append(all, c.stepDef)
	}
	return all
}

// Status returns the checklist for the workspace this admin is looking at.
//
// Never returns an error. Every probe already answers false when it cannot tell,
// and a setup checklist is not worth failing a dashboard load over: the cost of
// being wrong is one extra row shown to an admin who has already done that step,
// against a blank dashboard if this returned 500.
func Status(ctx context.Context, user userModels.UserInfo) State {
	features := helpers.FeatureStatus()

	skipped := map[string]bool{}
	for _, id := range loadSkipped() {
		skipped[id] = true
	}

	state := State{Dismissed: isDismissed()}
	managed := settingsBusiness.Managed()
	for _, def := range allDefinitions() {
		if !onTheList(def, features, managed) {
			continue
		}
		if def.applies != nil && !def.applies(ctx, user) {
			continue
		}
		step := def.Step
		// A set-aside step is not asked whether it is done. The probe is the
		// expensive part — one of them dials a model provider — and the answer
		// changes nothing that is shown.
		if skipped[step.ID] {
			step.Skipped = true
			state.Skipped++
			state.Steps = append(state.Steps, step)
			continue
		}
		step.Done = def.done(ctx, user)
		if step.Done {
			state.Done++
		}
		state.Total++
		state.Steps = append(state.Steps, step)
	}
	if state.Steps == nil {
		state.Steps = []Step{}
	}
	state.Complete = state.Done == state.Total
	return state
}

// onTheList decides whether a step applies to this build and this kind of
// workspace at all: one an absent subsystem contributes does not, and neither
// does one OneCamp Cloud does for a workspace it runs. Pure.
func onTheList(def stepDef, features map[string]bool, managed bool) bool {
	if def.include != nil && !def.include(features) {
		return false
	}
	return !(def.SelfHostedOnly && managed)
}

// Dismiss hides the checklist for the whole workspace, permanently.
func Dismiss() error {
	return configModels.UpsertConfig(dismissedKey, "true")
}

// SetStepSkipped sets a step aside, or puts it back.
//
// Only a step that declared itself skippable can be set aside, so a caller
// cannot make "invite your team" disappear by posting its id. Unknown ids are
// refused for the same reason: a typo should not write a row nothing will ever
// read again.
func SetStepSkipped(id string, skipped bool) error {
	if !isSkippable(id) {
		return fmt.Errorf("step %q cannot be set aside", id)
	}
	next := withMembership(loadSkipped(), id, skipped)
	return configModels.UpsertConfig(skippedKey, strings.Join(next, ","))
}

// isSkippable answers from the definitions rather than from a second list, so a
// step's own declaration is the only place this is decided.
func isSkippable(id string) bool {
	for _, def := range allDefinitions() {
		if def.ID == id {
			return def.Skippable
		}
	}
	return false
}

// loadSkipped reads the stored ids. A read failure reads as "nothing skipped":
// showing a step somebody set aside is a small annoyance, hiding one they never
// did is the checklist lying about what is left.
func loadSkipped() []string {
	cfg, err := configModels.GetConfigByKey(skippedKey)
	if err != nil || cfg == nil {
		return nil
	}
	return splitNonEmpty(cfg.Value)
}

// splitNonEmpty is strings.Split without the empty element an empty input gives,
// and without the blanks a trailing comma leaves behind.
func splitNonEmpty(raw string) []string {
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// withMembership returns the list with id present or absent as asked, preserving
// order and never duplicating. Generic over the operation because "skip" and
// "unskip" are the same edit in two directions, and writing them separately is
// how the two drift.
func withMembership(list []string, id string, present bool) []string {
	out := make([]string, 0, len(list)+1)
	found := false
	for _, v := range list {
		if v == id {
			found = true
			if !present {
				continue
			}
		}
		out = append(out, v)
	}
	if present && !found {
		out = append(out, id)
	}
	return out
}

// definitions are the built-in steps, in the order a person would actually do
// them: make a place to talk, make sure the invitations can leave the
// building, then get people into it. What comes after is contributed by the subsystems that
// exist on this build.
func definitions() []stepDef {
	return []stepDef{
		{
			// First, because it is about the account the rest is done from.
			// Shown only while the password is the one OneCamp Cloud emailed,
			// and gone once it is replaced: there is nothing to tick.
			Step: Step{
				ID:     "password",
				Title:  "Choose your own password",
				Detail: "You signed in with the password we emailed you. Replace it with one only you know.",
				Href:   "/app/profile",
			},
			applies: hasGeneratedPassword,
			done:    func(context.Context, userModels.UserInfo) bool { return false },
		},
		{
			Step: Step{
				ID:     "channel",
				Title:  "Create your first channel",
				Detail: "Channels are where conversations, files and calls live.",
				Href:   "/app/channel",
			},
			done: hasAnyChannel,
		},
		{
			Step: Step{
				ID:    "import",
				Title: "Bring your existing work over",
				Detail: "Slack history, or projects and tasks from Trello, Asana, Jira, monday.com, Notion, Linear, " +
					"ClickUp and Todoist. Doing it before people arrive means they land in a workspace that already has it.",
				Href: "/app/admin?tab=import",
				// NOT SLACK-SPECIFIC, and the first version of this step was.
				//
				// The import pipeline takes eight providers and the done probe has
				// always asked about all of them, but the row said "Bring your
				// Slack history over" and linked to the Slack tab. A team arriving
				// from Jira read a checklist that did not describe their migration
				// and a step they could only decline.
				//
				// Still skippable, because plenty of workspaces are starting from
				// nothing at all and no signal anywhere says which this is. Set
				// aside, it leaves the list; the card offers it back.
				Skippable: true,
			},
			done: hasImported,
		},
		{
			// Tasks, boards, forms, client links and time all live in a
			// project, and a new workspace has none. After import, which may have
			// brought projects across already. The link opens the dialog,
			// which also offers to create the team a project needs.
			Step: Step{
				ID:     "project",
				Title:  "Start your first project",
				Detail: "Projects hold tasks, boards, intake forms and the time spent on them, and can be shared with a client.",
				Href:   "/app/home?open=createProject",
			},
			done: hasAnyProject,
		},
		{
			// Before inviting anyone: without email an invitation is made and
			// never arrives. It came after "Invite your team", so an admin
			// working down the list invited people first and then wondered.
			Step: Step{
				ID:     "email",
				Title:  "Set up email",
				Detail: "Do this before you invite anyone: invitations, password resets and digests are all sent by email.",
				Href:   "/app/admin?tab=email-settings",
				// OneCamp Cloud lends a workspace it runs its email.
				SelfHostedOnly: true,
			},
			done: emailWorks,
		},
		{
			Step: Step{
				ID:     "people",
				Title:  "Invite your team",
				Detail: "A workspace with one person in it cannot show you much.",
				Href:   "/app/admin?tab=invitations",
			},
			done: hasOtherPeople,
		},
		// The AI steps are not here. "Connect a model provider" and "watch an
		// agent be refused" are registered by the AI packages through Register
		// below, so this package carries no AI knowledge and the AI-free edition
		// gets neither step by not linking them.
	}
}

// hasGeneratedPassword reports whether this admin still signs in with the
// password they were emailed. A read failure reads as no: this is a nudge, and
// a wrong one would nag an admin who already changed it.
var hasGeneratedPassword = func(ctx context.Context, user userModels.UserInfo) bool {
	generated, err := userDomain.PasswordGenerated(ctx, user.UserPostgresInfo.Id)
	return err == nil && generated
}

// hasAnyChannel asks whether this admin can see a channel, which is the question
// that matters: a channel they are not in would leave the dashboard just as empty.
func hasAnyChannel(ctx context.Context, user userModels.UserInfo) bool {
	channels, _, err := channelBusiness.GetUserActiveChannelListWithLatestPost(
		ctx, user.UserDgraphInfo.Uid, user.UserPostgresInfo.Id, 0, 1)
	return err == nil && len(channels) > 0
}

// hasAnyProject asks whether this admin runs a project. Only the projects they
// admin, which is what a new workspace's first admin's projects are.
func hasAnyProject(ctx context.Context, user userModels.UserInfo) bool {
	projects, err := projectBusiness.GetDgraphProjectListByAdminDgraphUID(ctx, user.UserDgraphInfo.Uid)
	return err == nil && len(projects) > 0
}

// hasImported reports whether any import has ever been started here.
//
// Any provider and any outcome, deliberately. The question the step asks is
// "have you brought your work across", and it is answered by ListJobs with no
// provider filter, so a workspace that came from Jira counts exactly as much as
// one that came from Slack. An import that was attempted and failed also counts:
// that is not a workspace which still needs to be told the feature exists, it is
// one whose admin is already in the middle of it.
func hasImported(ctx context.Context, _ userModels.UserInfo) bool {
	jobs, err := importDomain.ListJobs(ctx, "", 1)
	return err == nil && len(jobs) > 0
}

// hasOtherPeople asks for two users and checks it got two. Cheaper than a count
// and it answers the actual question, which is "is anybody else here".
func hasOtherPeople(ctx context.Context, _ userModels.UserInfo) bool {
	users, _, err := userBusiness.GetAllUsersListFromDgraph(ctx, 0, 2)
	return err == nil && len(users) > 1
}

// emailWorks reports whether outbound mail is configured. Without it an invitation
// is created and never arrives, which looks to the admin like the invite failed
// and to the invitee like nothing happened.
// Imported directly rather than through the feature registry, unlike AI: email
// is present in BOTH editions, so there is no edition to hide it from and the
// inversion would buy nothing.
func emailWorks(_ context.Context, _ userModels.UserInfo) bool {
	return emailService.IsEmailEnabled()
}

// isDismissed reads the workspace flag, treating any read failure as "not
// dismissed". Showing a checklist that was dismissed is a small annoyance;
// hiding one that was never dismissed defeats the whole feature.
func isDismissed() bool {
	cfg, err := configModels.GetConfigByKey(dismissedKey)
	return err == nil && cfg != nil && cfg.Value == "true"
}
