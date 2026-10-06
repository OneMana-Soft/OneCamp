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

// projectUpdatePrompt is what the model reads: the facts, and the lead's
// last updates for their voice. Pure.
func projectUpdatePrompt(project, facts string, previous []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Project: %s\n\nFacts:\n%s\n", project, facts)
	if len(previous) > 0 {
		b.WriteString("\nPrevious updates, newest first:\n")
		for i, p := range previous {
			fmt.Fprintf(&b, "--- %d ---\n%s\n", i+1, p)
		}
	}
	return b.String()
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
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return out, nil
	}
	if err := svc.Resiliency.CB.Allow(); err != nil {
		return out, nil
	}
	var previous []string
	if prev, err := updateBusiness.List(ctx, projectID, 3, false); err == nil {
		for _, p := range prev {
			previous = append(previous, helpers.OneLine(p.Body, 1200))
		}
	}
	summary, err := svc.Summarize(ctx, projectUpdatePrompt(d.Facts.Project, d.Text, previous), projectUpdateSystemPrompt)
	if err != nil {
		svc.Resiliency.CB.RecordResult(err)
		return out, nil
	}
	svc.Resiliency.CB.RecordSuccess()
	summary = strings.TrimSpace(SanitizeResponse(summary))
	if summary == "" {
		return out, nil
	}
	out.Text = helpers.NormaliseText(summary) + "\n\n" + d.Text
	out.AI = true
	return out, nil
}
