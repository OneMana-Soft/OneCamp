package business

// THE GOVERNANCE DRILL: one button that proves the product's central claim, or
// proves it is broken.
//
// The claim is on the homepage: an agent can only do what the person behind it
// could, the decision is written before the tool runs, and refusals are on the
// record in a hash chain. Every part of that is implemented and none of it is
// VISIBLE. A stranger can finish the whole demo without seeing a single refusal,
// which means the one thing a competitor cannot copy next month is also the one
// thing nobody is ever shown.
//
// DETERMINISTIC, SO NO MODEL. The obvious build is "ask an agent to post in
// #finance and watch it get refused", and it is the wrong one: whether a model
// chooses to attempt the call is a coin toss, and a demo that sometimes proves
// nothing is worse than no demo. The guard being demonstrated is not the model's
// judgement, it is the permission check and the audit ordering, so the drill
// drives those directly, through the SAME executor a real tool call goes through
// with the same params. Same code path, same refusal, every time.
//
// IT FAILS CLOSED, AND THAT IS THE POINT. If the forbidden post SUCCEEDS, this
// install is not enforcing the guarantee and the drill says so in those words. If
// the audit row cannot be written, the action is not attempted at all, because "if
// it can't be recorded, it doesn't happen" is a guarantee and not a slogan. A
// drill that passed while the guarantees were broken would be the most expensive
// feature in the product.
//
// NAMESPACED FIXTURES. The channels are #drill-engineering and #drill-finance,
// never #finance. Customers run this on a real workspace, and a demo that
// collides with the channel their finance team actually uses is a demo that did
// harm. The story keeps the short names; the product does not.

import (
	"context"
	"fmt"
	"time"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	auditModel "github.com/akashc777/OneCamp/models/postgres/AdminAudit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
)

const (
	// AllowedChannel is the channel the principal IS in. It exists so a reader can
	// see the refusal is about this channel and not about the agent being unable
	// to post anywhere at all.
	AllowedChannel = "drill-engineering"
	// ForbiddenChannel is the channel the principal is NOT in. The whole
	// assertion is that a post here is refused.
	ForbiddenChannel = "drill-finance"

	// actionPrefix is what the read-back filters on, so the drill's own rows can
	// be found without scanning the log.
	actionPrefix       = "agent.drill."
	auditActionAttempt = actionPrefix + "attempt"
	auditActionRefused = actionPrefix + "refused"
	auditActionEscaped = actionPrefix + "not_refused"

	drillMessage = "Governance drill. If you can read this in the channel, the permission check did not hold."

	// How much of the chain the drill recomputes. Big enough that the answer means
	// something about real history rather than only about the two rows just
	// written, small enough to stay a request rather than a report.
	chainVerifyWindow = 500
)

// StepResult is one line of the drill as a reader follows it.
type StepResult struct {
	Name string `json:"name"`
	// Explain says what this step proves. A green tick whose scope nobody states
	// is worth less than nothing, which is the lesson the system checks already
	// paid for.
	Explain string `json:"explain"`
	OK      bool   `json:"ok"`
	Detail  string `json:"detail,omitempty"`
}

// AuditRow is a real row from the log, quoted back so the UI can show the
// sequence number and hash rather than asking the viewer to take this on trust.
type AuditRow struct {
	Seq     int64  `json:"seq"`
	ID      string `json:"id"`
	Action  string `json:"action"`
	Summary string `json:"summary"`
	// PrevHash and EntryHash are shown as a pair, because one hash on its own
	// demonstrates nothing. The link is the claim: this row carries the previous
	// row's fingerprint, so removing or editing anything behind it changes what
	// this row should have hashed to.
	PrevHash  string    `json:"prev_hash,omitempty"`
	EntryHash string    `json:"entry_hash,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// DrillResult is the whole answer.
type DrillResult struct {
	// Passed is true only when the forbidden action was refused AND the refusal
	// was recorded AND the chain verifies. Anything else is a finding.
	Passed bool         `json:"passed"`
	Steps  []StepResult `json:"steps"`
	// RefusalReason is the sentence the permission layer produced, quoted rather
	// than paraphrased: a demo that rewords the refusal invites the question of
	// whether the refusal was real.
	RefusalReason string `json:"refusal_reason,omitempty"`
	// Rows are this run's own audit entries, newest last.
	Rows         []AuditRow `json:"rows,omitempty"`
	ChainOK      bool       `json:"chain_ok"`
	ChainChecked int        `json:"chain_checked"`
	ChainMessage string     `json:"chain_message,omitempty"`
	// ChainPartial says the drill checked a WINDOW of the log rather than all of
	// it, and ChainFromSeq says where that window began. Carried through to the UI
	// instead of being smoothed over: "the last 500 entries verify" and "the log
	// has not been altered" are different claims, and a compliance demo that
	// quietly makes the larger one is the exact failure it exists to prevent.
	ChainPartial bool      `json:"chain_partial"`
	ChainFromSeq int64     `json:"chain_from_seq,omitempty"`
	RanAt        time.Time `json:"ran_at"`
}

// Seeded reports whether the fixture exists, so a caller can offer "set up the
// drill" instead of an error a reader has to interpret.
func Seeded(ctx context.Context) bool {
	name := ForbiddenChannel
	exists, err := channelBusiness.CheckIfChannelExist(ctx, &name)
	return err == nil && exists
}

// Seed creates the fixture. Safe to call repeatedly.
//
// The forbidden channel is created BY the admin and the admin's membership edge
// is then removed, which is how a channel nobody is in comes to exist: creating
// one necessarily makes you a member, and ch_is_member is a live count of that
// edge. Removal uses the same call the admin's own "remove member" button uses,
// so the fixture is built out of the product rather than around it.
func Seed(ctx context.Context, admin userModels.UserInfo) error {
	if Seeded(ctx) {
		return nil
	}

	// The allowed channel is created first and independently: if it already
	// exists (a re-seed after a partial failure) that is not an error.
	allowed := AllowedChannel
	if exists, err := channelBusiness.CheckIfChannelExist(ctx, &allowed); err != nil {
		return fmt.Errorf("could not check for #%s: %w", AllowedChannel, err)
	} else if !exists {
		if err, _ := channelBusiness.CreateChannel(ctx, &channelAdapter.InputCreateChannel{
			ChannelName:    AllowedChannel,
			ChannelPrivate: true,
		}, &admin); err != nil {
			return fmt.Errorf("could not create #%s: %w", AllowedChannel, err)
		}
	}

	err, forbiddenUUID := channelBusiness.CreateChannel(ctx, &channelAdapter.InputCreateChannel{
		ChannelName:    ForbiddenChannel,
		ChannelPrivate: true,
	}, &admin)
	if err != nil {
		return fmt.Errorf("could not create #%s: %w", ForbiddenChannel, err)
	}

	ch, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, forbiddenUUID, admin.UserDgraphInfo.Uid)
	if err != nil || ch == nil || ch.Uid == "" {
		return fmt.Errorf("created #%s but could not read it back, so the fixture is half built: %w",
			ForbiddenChannel, err)
	}

	// The step that makes the fixture a fixture. Without it the admin is in the
	// channel they just made and nothing would be refused, so a failure here has
	// to be reported rather than logged: a drill that passes for the wrong reason
	// is the one outcome worse than a drill that fails.
	if err := channelBusiness.DeleteChannelMemberEdge(
		ctx, ch.Uid, admin.UserDgraphInfo.Uid,
		admin.UserPostgresInfo.Id.String(), forbiddenUUID.String()); err != nil {
		return fmt.Errorf("could not leave #%s, so the drill would pass for the wrong reason: %w",
			ForbiddenChannel, err)
	}
	return nil
}

// Run performs the drill as the given principal.
func Run(ctx context.Context, principal userModels.UserInfo) (*DrillResult, error) {
	ctx = drillContext(ctx)
	res := &DrillResult{RanAt: time.Now().UTC()}

	ch, err := resolveDrillChannel(ctx, ForbiddenChannel, principal.UserDgraphInfo.Uid)
	if err != nil {
		return nil, err
	}

	// STEP 1. The principal must genuinely lack membership, or nothing below
	// proves anything. Read through the SAME cached query the executor uses, so
	// the precondition and the enforcement cannot disagree — and checked rather
	// than assumed, because a fixture that has drifted is the one way this could
	// quietly lie.
	notMember := ch.IsMember == 0
	res.Steps = append(res.Steps, StepResult{
		Name: "the person is not a member",
		Explain: fmt.Sprintf("#%s exists and the person running this is not in it. "+
			"Without that, a refusal would not mean anything.", ForbiddenChannel),
		OK: notMember,
		Detail: detailIf(!notMember, fmt.Sprintf(
			"the fixture has drifted: this person IS a member of #%s, so nothing below is a real test. "+
				"Set the drill up again.", ForbiddenChannel)),
	})
	if !notMember {
		return res, nil
	}

	// STEP 2. Record the intent BEFORE the attempt, and refuse to attempt if the
	// record cannot be written. The ordering guarantee, executed rather than
	// described.
	attemptErr := recordDrill(ctx, principal, auditActionAttempt,
		fmt.Sprintf("Drill: an agent acting for %s is about to attempt a post in #%s",
			principal.UserPostgresInfo.EmailID, ForbiddenChannel),
		map[string]interface{}{"channel": ForbiddenChannel, "tool": "send_message", "phase": "before"})
	res.Steps = append(res.Steps, StepResult{
		Name: "the attempt is recorded before it runs",
		Explain: "The audit row is written first. If that write fails the call is abandoned, " +
			"because a decision nobody recorded is worse than one recorded and never made.",
		OK:     attemptErr == nil,
		Detail: detailIf(attemptErr != nil, "the audit write failed, so the action was not attempted: "+errText(attemptErr)),
	})
	if attemptErr != nil {
		return res, nil
	}

	// STEP 3. Attempt it, through the executor a real agent tool call goes through.
	exec, ok := ai.GetExecutor("send_message")
	if !ok {
		res.Steps = append(res.Steps, StepResult{
			Name:    "the agent attempts the post",
			Explain: "The same executor an agent's tool call goes through, so this is not a simulation of the check.",
			OK:      false,
			Detail:  "the send_message tool is not registered in this build, so the drill cannot run",
		})
		return res, nil
	}
	_, _, execErr := exec(ctx, ai.ProposedAction{
		ToolName:    "send_message",
		Description: "Governance drill: attempt a post in a channel the principal is not in",
		Params: map[string]string{
			"channel_uuid": ch.Uuid,
			"text":         drillMessage,
		},
	}, principal.UserPostgresInfo.Id.String())

	refused := execErr != nil
	if refused {
		res.RefusalReason = execErr.Error()
	}
	res.Steps = append(res.Steps, StepResult{
		Name: "the post is refused",
		Explain: "Checked against the person's live channel membership at the moment of the call, " +
			"not against a grant cached when the agent was set up.",
		OK: refused,
		Detail: detailIf(!refused, fmt.Sprintf(
			"THE POST SUCCEEDED. A message was written to #%s on behalf of somebody who is not a "+
				"member, so this install is not enforcing the rule it claims to. Treat this as an "+
				"incident, not a failed test.", ForbiddenChannel)),
	})

	// STEP 4. Record the outcome either way. A refusal nobody can find is the
	// failure this product exists to remove, and an escape that goes unrecorded
	// is worse.
	action, summary := auditActionRefused,
		fmt.Sprintf("Drill: post in #%s refused (%s)", ForbiddenChannel, res.RefusalReason)
	if !refused {
		action = auditActionEscaped
		summary = fmt.Sprintf("Drill: post in #%s WAS NOT REFUSED; this install is not enforcing channel membership",
			ForbiddenChannel)
	}
	outcomeErr := recordDrill(ctx, principal, action, summary, map[string]interface{}{
		"channel": ForbiddenChannel, "tool": "send_message", "phase": "after",
		"refused": refused, "reason": res.RefusalReason,
	})
	res.Steps = append(res.Steps, StepResult{
		Name: "the refusal is on the record",
		Explain: "A denied call leaves a row naming the reason, the tool, and the person behind it, " +
			"so an auditor can find what did not happen.",
		OK:     outcomeErr == nil,
		Detail: detailIf(outcomeErr != nil, "the outcome could not be recorded: "+errText(outcomeErr)),
	})

	// STEP 5. The chain, recomputed against the log itself so a reader does not
	// have to trust this page.
	//
	// Windowed, not the whole log. Verify walks every hashed row, which is right
	// for an auditor and wrong for a button in a browser: the log only grows, so a
	// call that is instant on a fresh install becomes a slow query and then a
	// gateway timeout on a workspace that has been running a year. The roadmap
	// named this risk before the drill existed. The window is far larger than the
	// two rows this run wrote, so it always covers them plus real history, and the
	// result says which of the two claims it is making.
	v, verr := auditBusiness.VerifyRecent(ctx, chainVerifyWindow)
	chainOK := verr == nil && v != nil && v.OK
	if v != nil {
		res.ChainChecked, res.ChainMessage = v.Checked, v.Message
		res.ChainPartial, res.ChainFromSeq = v.Partial, v.FromSeq
	}
	res.ChainOK = chainOK
	res.Steps = append(res.Steps, StepResult{
		Name:    "the log still verifies",
		Explain: chainExplain(res.ChainPartial, res.ChainChecked),
		OK:      chainOK,
		Detail:  detailIf(!chainOK, "the chain did not verify: "+helpers.FirstNonEmpty(errText(verr), res.ChainMessage, "no detail")),
	})

	// The rows themselves, read back rather than constructed, so the sequence
	// numbers and hashes in the UI are the ones in the database.
	res.Rows = readBackRows(ctx, principal.UserPostgresInfo.EmailID, res.RanAt)
	res.Passed = allOK(res.Steps)
	return res, nil
}

// resolveDrillChannel turns a drill channel name into the same per-viewer
// summary every permission gate reads.
func resolveDrillChannel(ctx context.Context, name, viewerDgraphUID string) (*dgraphStruct.DgraphChannel, error) {
	row, err := channelDomain.GetChannelByName(ctx, name)
	if err != nil || row == nil {
		return nil, fmt.Errorf("#%s does not exist yet; set the drill up first", name)
	}
	ch, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, row.Id, viewerDgraphUID)
	if err != nil || ch == nil || ch.Uuid == "" {
		return nil, fmt.Errorf("#%s exists in Postgres but not in the graph; set the drill up again: %w", name, err)
	}
	if ch.DeletedAt != nil && !ch.DeletedAt.IsZero() {
		return nil, fmt.Errorf("#%s has been archived; set the drill up again", name)
	}
	return ch, nil
}

// drillContext says who started the drill: the person who pressed the button.
//
// The rows below are written as the agent, because an agent is what acted, and
// an agent row with no initiator reads as "nobody said". Somebody did. A drill
// is the one agent action that is never unattended, and a reviewer filtering
// for what ran with nobody watching must not find the admin's own rehearsal.
func drillContext(ctx context.Context) context.Context {
	return auditBusiness.WithInitiator(ctx, auditBusiness.InitiatorPerson)
}

// recordDrill writes one audit row as the agent principal.
func recordDrill(ctx context.Context, principal userModels.UserInfo, action, summary string,
	meta map[string]interface{}) error {
	id := principal.UserPostgresInfo.Id
	meta["drill"] = true
	// ActorAgent, not the admin: the accountable human is the actor id, and the
	// thing that acted was an agent. Recording it as a person would be the
	// comfortable lie this log exists to prevent.
	return auditBusiness.RecordForPrincipal(ctx, &id, principal.UserPostgresInfo.EmailID,
		auditBusiness.ActorAgent, action, auditBusiness.CategoryAgent, summary, meta)
}

// readBackRows fetches this run's entries. Filtered by actor and by time rather
// than just taking the newest two, so two people drilling at once cannot be shown
// each other's evidence. Best effort: the steps above already carry the verdict,
// and failing the drill because a display query failed would be its own lie.
func readBackRows(ctx context.Context, actorEmail string, since time.Time) []AuditRow {
	entries, err := auditModel.ListByActionPrefixes(ctx, []string{actionPrefix}, nil, 20)
	if err != nil {
		return nil
	}
	cutoff := since.Add(-time.Second)
	out := make([]AuditRow, 0, 2)
	for i := len(entries) - 1; i >= 0; i-- { // oldest-first, the chain's own order
		e := entries[i]
		if e.ActorEmail != actorEmail || e.CreatedAt.Before(cutoff) {
			continue
		}
		out = append(out, AuditRow{
			Seq: e.Seq, ID: e.Id.String(), Action: e.Action, Summary: e.Summary,
			PrevHash: e.PrevHash, EntryHash: e.EntryHash, CreatedAt: e.CreatedAt,
		})
	}
	return out
}

// chainExplain states what this step actually checked. A window and a full walk
// support different claims, so they get different sentences.
func chainExplain(partial bool, checked int) string {
	base := "Each entry hashes the one before it, so recomputing the chain is how an edit to " +
		"history is detected rather than trusted not to happen. "
	if partial {
		return base + fmt.Sprintf("This checked the most recent %d entries, including the two just "+
			"written. Use Verify audit log for the whole chain from its first entry.", checked)
	}
	return base + "This checked the whole chain, from its first entry."
}

func allOK(steps []StepResult) bool {
	for _, s := range steps {
		if !s.OK {
			return false
		}
	}
	return len(steps) > 0
}

func detailIf(cond bool, msg string) string {
	if cond {
		return msg
	}
	return ""
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
