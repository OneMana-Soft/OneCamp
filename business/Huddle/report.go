package business

// A call's report (calls package A, "Ask the room": meet-10, zoom-02): once a
// conversation's call ends, one reply under its post says what its polls
// found and what was asked. It lists each poll's counts (unless whoever ran
// the call kept them back), and the questions by upvotes, with the asker's
// name only where they didn't ask anonymously: every question, or where
// whoever ran the call showed only the answered ones, only those, since
// everyone who reads the conversation reads the report. The boards it showed
// on its stage (calls package B) are linked by link alone: opening one
// follows the board's own rules, and its title stays with whoever may read
// it. A call with none of these gets nothing.
//
// It's written once:
//   - the call's row is claimed (a lease, so a writer that died is followed)
//     and marked written after;
//   - the write itself goes through Send's Once, keyed by the call, so a
//     retry after a write whose mark was lost gets the first answer back;
//   - the end's sink writes it, and the sweep writes any the sink missed.
//
// It's written as the call's starter, under the thread's own reply rules
// (business/CallThread's Report). A guest never reads it: their reads of a
// call's thread stop at the call's end, and the report comes after. It
// reaches the work graph as any reply does: nothing new.

import (
	"context"
	"errors"
	"fmt"
	"html"
	"strings"
	"time"

	callQABusiness "github.com/akashc777/OneCamp/business/CallQA"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	callThreadBusiness "github.com/akashc777/OneCamp/business/CallThread"
	pollBusiness "github.com/akashc777/OneCamp/business/Poll"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	"github.com/akashc777/OneCamp/helpers"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	pollModel "github.com/akashc777/OneCamp/models/postgres/Poll"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "calls.huddle.reports", Every: time.Minute, Run: repairReports})
}

const (
	// reportLease is how long a claim to write a report keeps others off it.
	reportLease = 2 * time.Minute
	// reportWithin is how far back the sweep looks for a report not written.
	reportWithin = 24 * time.Hour
	// reportAPass is how many calls one sweep pass takes.
	reportAPass = 50
	// reportQuestions is the most questions a report lists, by upvotes.
	reportQuestions = 25
)

// The report's reads and writes, seams for tests.
var reportSeams = struct {
	claim     func(context.Context, uuid.UUID, time.Duration) (bool, error)
	done      func(context.Context, uuid.UUID) error
	due       func(context.Context, time.Time, time.Time, time.Duration, int) ([]uuid.UUID, error)
	polls     func(context.Context, uuid.UUID) ([]*pollBusiness.View, error)
	questions func(context.Context, uuid.UUID, string) ([]callQABusiness.View, error)
	boards    func(context.Context, uuid.UUID) ([]uuid.UUID, error)
	write     func(context.Context, *userModels.UserInfo, *callStateBusiness.Session, string) error
}{
	claim: callStateModel.ClaimReport,
	done:  callStateModel.ReportDone,
	due:   callStateModel.ReportsDue,
	polls: func(ctx context.Context, session uuid.UUID) ([]*pollBusiness.View, error) {
		// Every count, as whoever ran the call saw them: reportHTML leaves
		// out the ones they kept back.
		return pollBusiness.CallViews(ctx, session, func(*pollModel.Poll) (uuid.UUID, error) { return uuid.Nil, nil }, true)
	},
	questions: func(ctx context.Context, session uuid.UUID, shown string) ([]callQABusiness.View, error) {
		// As someone in the call who doesn't run it saw them, by the call's
		// last choice of all or answered only.
		return callQABusiness.Views(ctx, session, "", false, shown)
	},
	boards: callStateModel.BoardsShown,
	write:  callThreadBusiness.Report,
}

// report writes an ended call's report under its post, once.
func report(ctx context.Context, s *callStateBusiness.Session) error {
	id, err := uuid.Parse(s.ID)
	if err != nil || s.Live || s.PostUUID == "" {
		return nil
	}
	claimed, err := reportSeams.claim(ctx, id, reportLease)
	if err != nil || !claimed {
		return err
	}
	polls, err := reportSeams.polls(ctx, id)
	if err != nil {
		return err
	}
	questions, err := reportSeams.questions(ctx, id, s.QA.Shown)
	if err != nil {
		return err
	}
	boards, err := reportSeams.boards(ctx, id)
	if err != nil {
		return err
	}
	if body := reportHTML(polls, questions, boards); body != "" {
		if err := writeReport(ctx, s, body); err != nil {
			// The claim lapses, and the sweep tries again.
			return err
		}
	}
	return reportSeams.done(ctx, id)
}

// writeReport writes body under the call's post as its starter, once a call
// (Send's Once). A starter who may no longer reply there, or a post that's
// gone, leaves the report nowhere to go: it's said, not tried again.
func writeReport(ctx context.Context, s *callStateBusiness.Session, body string) error {
	starter, err := uuid.Parse(s.StartedBy)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Huddle: call %s's report has no starter to write it", s.ID)
		return nil
	}
	who, err := seams.actor(ctx, starter)
	if err != nil {
		return err
	}
	_, err = sendBusiness.Once(ctx, s.StartedBy, s.PostUUID, "call-report-"+s.ID, func() (bool, error) {
		return true, reportSeams.write(ctx, who, s, body)
	})
	if errors.Is(err, callThreadBusiness.ErrReadOnly) || errors.Is(err, callThreadBusiness.ErrNotFound) {
		helpers.LogErrorWithContext(ctx, "business/Huddle: call %s's report has nowhere to go: %v", s.ID, err)
		return nil
	}
	return err
}

// repairReports writes the reports the end's sink missed: calls that ended
// in the last day, a while ago, with a post and no report written.
func repairReports(ctx context.Context) error {
	now := seams.now()
	ids, err := reportSeams.due(ctx, now.Add(-reportWithin), now.Add(-repairAfter), reportLease, reportAPass)
	if err != nil {
		return err
	}
	for _, id := range ids {
		s, err := seams.session(ctx, id.String())
		if err != nil || s == nil {
			continue
		}
		if err := report(ctx, s); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Huddle: call %s's report (the sweep tries again): %+v", id, err)
		}
	}
	return nil
}

// reportHTML is a call's report as a message: its polls' counts, and its
// questions by upvotes with the askers' names where they gave them, and links
// to the boards it showed. "" for a call with none. Everything people wrote is escaped. Pure.
func reportHTML(polls []*pollBusiness.View, questions []callQABusiness.View, boards []uuid.UUID) string {
	if len(polls) == 0 && len(questions) == 0 && len(boards) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<p><strong>From the call</strong></p>")
	for _, p := range polls {
		fmt.Fprintf(&b, "<p><strong>Poll:</strong> %s</p>", html.EscapeString(p.Question))
		if p.ResultsHidden {
			b.WriteString("<p>The results stayed with the people running the call.</p>")
			continue
		}
		b.WriteString("<ul>")
		for _, o := range p.Options {
			fmt.Fprintf(&b, "<li>%s: %s</li>", html.EscapeString(o.Text), plural(o.Votes, "vote", "votes"))
		}
		b.WriteString("</ul>")
		note := plural(p.Voters, "person voted", "people voted")
		if p.Anonymous {
			note += ", anonymously"
		}
		fmt.Fprintf(&b, "<p>%s.</p>", note)
	}
	if len(questions) > 0 {
		b.WriteString("<p><strong>Questions</strong></p><ul>")
		for i, q := range questions {
			if i == reportQuestions {
				break
			}
			line := html.EscapeString(q.Text) + " (" + plural(q.Votes, "upvote", "upvotes")
			if q.State == callQABusiness.StateAnswered {
				line += ", answered"
			}
			if q.Asker != "" {
				line += ", asked by " + html.EscapeString(q.Asker)
			}
			fmt.Fprintf(&b, "<li>%s)</li>", line)
		}
		b.WriteString("</ul>")
		if more := len(questions) - reportQuestions; more > 0 {
			fmt.Fprintf(&b, "<p>And %s.</p>", plural(more, "more question", "more questions"))
		}
	}
	switch len(boards) {
	case 0:
	case 1:
		fmt.Fprintf(&b, `<p><a href="/app/board/%s">Board from this call</a></p>`, boards[0])
	default:
		b.WriteString("<p><strong>Boards from this call</strong></p><ul>")
		for i, id := range boards {
			fmt.Fprintf(&b, `<li><a href="/app/board/%s">Board %d</a></li>`, id, i+1)
		}
		b.WriteString("</ul>")
	}
	return b.String()
}

// plural is a count with its noun: "1 vote", "3 votes". Pure.
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
