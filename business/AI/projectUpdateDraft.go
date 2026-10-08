package business

// The AI's part in a project update: a two or three sentence summary, in the
// voice of the team's last updates, placed above the factual draft (what was
// done, what is stuck or late, what is next) that business/ProjectUpdate
// writes from the project's tasks. As with the team report, the facts are
// read, not generated: the model only narrates them, and if it can't, the
// person still gets the facts.

import (
	"context"
	"fmt"
	"strings"
	"time"

	updateBusiness "github.com/akashc777/OneCamp/business/ProjectUpdate"
	"github.com/akashc777/OneCamp/helpers"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

const projectUpdateSystemPrompt = `You write the opening of a project update that a team lead will post.
You are given the project's facts for the period (finished, in progress, stuck, overdue, due soon, time logged) and, when there are any, the lead's previous updates.
Write two or three short sentences: what moved, the main risk if there is one, and what comes next.
Write in the voice and tone of the previous updates when there are some.
Use only the facts given. Never invent names, dates, numbers, reasons or blockers.
Plain text only: no headings, no lists, no emoji, no greeting, no sign-off.
If nothing happened in the period, say so in one sentence.`

// factsPrompt is what the model reads for a summary of a factual draft: what
// it is about ("Project: Q4 launch"), the facts, and the author's last few
// posts of the kind ("updates", "check-ins") for their voice. Pure.
func factsPrompt(subject, name, facts, kind string, previous []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n\nFacts:\n%s\n", subject, name, facts)
	if len(previous) > 0 {
		fmt.Fprintf(&b, "\nPrevious %s, newest first:\n", kind)
		for i, p := range previous {
			fmt.Fprintf(&b, "--- %d ---\n%s\n", i+1, p)
		}
	}
	return b.String()
}

// summaryOnTop puts the model's short summary of a factual draft above the
// facts. The prompt is only built when there is a model to read it. Without a
// working AI, or when it says nothing, the facts come back alone (false): the
// person always gets them.
func summaryOnTop(ctx context.Context, system, facts string, prompt func() string) (string, bool) {
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return facts, false
	}
	if err := svc.Resiliency.CB.Allow(); err != nil {
		return facts, false
	}
	summary, err := svc.Summarize(ctx, prompt(), system)
	if err != nil {
		svc.Resiliency.CB.RecordResult(err)
		return facts, false
	}
	svc.Resiliency.CB.RecordSuccess()
	summary = strings.TrimSpace(SanitizeResponse(summary))
	if summary == "" {
		return facts, false
	}
	return helpers.NormaliseText(summary) + "\n\n" + facts, true
}

// AIDraft is a draft with the AI's summary on top, and whether it got one.
type AIDraft struct {
	updateBusiness.Draft
	AI bool `json:"ai"`
}

// DraftProjectUpdate drafts a project's next update with an AI summary above
// the facts. Without a working AI it returns the facts alone (AI false).
func DraftProjectUpdate(ctx context.Context, projectID uuid.UUID, userDgraphUID string, now time.Time) (*AIDraft, error) {
	d, err := updateBusiness.MakeDraft(ctx, projectID, userDgraphUID, now)
	if err != nil {
		return nil, err
	}
	out := &AIDraft{Draft: *d}
	out.Text, out.AI = summaryOnTop(ctx, projectUpdateSystemPrompt, d.Text, func() string {
		var previous []string
		if prev, err := updateBusiness.List(ctx, projectID, 3, false); err == nil {
			for _, p := range prev {
				previous = append(previous, helpers.OneLine(p.Body, 1200))
			}
		}
		return factsPrompt("Project", d.Facts.Project, d.Text, "updates", previous)
	})
	return out, nil
}
