package business

// The AI's part in a goal check-in: two or three sentences above the factual
// draft business/Goal writes from the goal's projects, sub-goals and number.
// As with project updates, the facts are read, not generated: the model only
// narrates them, and if it can't, the owner still gets the facts.

import (
	"context"
	"time"

	goalBusiness "github.com/akashc777/OneCamp/business/Goal"
	"github.com/akashc777/OneCamp/helpers"
	goalModel "github.com/akashc777/OneCamp/models/postgres/Goal"
	"github.com/google/uuid"
)

const goalCheckInSystemPrompt = `You write the opening of a check-in on a team goal that its owner will post.
You are given the goal's facts: its progress and how it moved since the last check-in, how much of its time has passed, and how each project and sub-goal serving it is doing; and, when there are any, the owner's previous check-ins.
Write two or three short sentences: where the goal stands, the main risk if there is one, and what would move it next.
Write in the voice and tone of the previous check-ins when there are some.
Use only the facts given. Never invent names, dates, numbers, reasons or blockers.
Plain text only: no headings, no lists, no emoji, no greeting, no sign-off.`

// AIGoalDraft is a check-in draft with the AI's summary on top, and whether it got one.
type AIGoalDraft struct {
	goalBusiness.Draft
	AI bool `json:"ai"`
}

// DraftGoalCheckIn drafts a goal's next check-in with an AI summary above the
// facts. Without a working AI it returns the facts alone (AI false).
func DraftGoalCheckIn(ctx context.Context, r goalBusiness.Reader, goalID uuid.UUID, now time.Time) (*AIGoalDraft, error) {
	d, err := goalBusiness.MakeDraft(ctx, r, goalID, now)
	if err != nil {
		return nil, err
	}
	out := &AIGoalDraft{Draft: *d}
	out.Text, out.AI = summaryOnTop(ctx, goalCheckInSystemPrompt, d.Text, func() string {
		var previous []string
		if list, err := goalModel.CheckIns(goalID, 3); err == nil {
			for _, c := range list {
				if c.Body != "" {
					previous = append(previous, helpers.OneLine(c.Body, 1200))
				}
			}
		}
		return factsPrompt("Goal", d.Facts.Goal, d.Text, "check-ins", previous)
	})
	return out, nil
}
