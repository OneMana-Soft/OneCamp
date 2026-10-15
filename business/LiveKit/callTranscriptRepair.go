package business

// Transcripts of calls nobody recorded, kept before calls had an end.
//
// Each such call's words hung off a session node that was never stamped as
// ended, and every list, the delete endpoints and the retention policy take
// only ended recordings: the words of every unrecorded call were kept for
// ever, invisibly, whatever the workspace's retention said. Instant meetings
// were worse: their lines landed on a node linked to no conversation at all.
//
// Calls get an end now (EndCallSession, at room_finished). This finishes the
// ones from before, once per start, idempotently:
//   - a node linked to its channel or DM, whose call is long over, gets its end
//     (the time of its last line), so it is listed with the conversation's
//     recordings, its people can delete it, and retention reaches it;
//   - a node linked to nothing can't be attached (it does not say which room it
//     was), and nobody can reach it. It is counted and logged, never deleted:
//     removing it is the owner's decision (calls O3), and it may be a real
//     conversation's transcript whose link failed once. If a purge comes back,
//     it deletes by the node's uid, never by its key (a key can name several
//     nodes, and one of them linked).

import (
	"context"
	"fmt"
	"time"

	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
)

const (
	// endLinkedAfter is how long after its last line a linked call is
	// certainly over (a call can't run longer than a token's room).
	endLinkedAfter = 6 * time.Hour
	// unreachableAfter is how long after its last line a node nobody can reach
	// is reported.
	unreachableAfter = 24 * time.Hour
	// repairPage is how many nodes one read takes.
	repairPage = 200
)

type sessionRepair int

const (
	repairNothing sessionRepair = iota
	repairEnd
	repairUnreachable
)

// decideSessionRepair is what the repair does with one node. Pure.
func decideSessionRepair(n recordingDomain.CallSessionNode, now time.Time) sessionRepair {
	if !helpers.IsCallSessionKey(n.Key) || n.DeletedAt.After(epoch) {
		return repairNothing
	}
	last := n.LastActivity()
	if n.Linked {
		if n.EndedAt.After(epoch) || now.Sub(last) < endLinkedAfter {
			return repairNothing
		}
		return repairEnd
	}
	if now.Sub(last) < unreachableAfter {
		return repairNothing
	}
	return repairUnreachable
}

var epoch = time.Unix(0, 0)

// CallTranscriptRepair says what one run did.
type CallTranscriptRepair struct {
	Ended, Unreachable int
}

func (r CallTranscriptRepair) String() string {
	return fmt.Sprintf("%d call transcripts ended, %d linked to no conversation (kept)", r.Ended, r.Unreachable)
}

// The seams the repair's tests replace.
var (
	callSessionNodes     = recordingDomain.CallSessionNodes
	markCallSessionEnded = recordingDomain.MarkCallSessionNodeEnded
)

// RepairCallTranscripts runs the repair over every node with a call's words
// and no media.
func RepairCallTranscripts(ctx context.Context, now time.Time) (CallTranscriptRepair, error) {
	var rep CallTranscriptRepair
	after := "0x0"
	for {
		page, err := callSessionNodes(ctx, after, repairPage)
		if err != nil {
			return rep, err
		}
		for _, n := range page {
			switch decideSessionRepair(n, now) {
			case repairEnd:
				if err := markCallSessionEnded(ctx, n.Key, n.LastActivity()); err != nil {
					return rep, err
				}
				rep.Ended++
			case repairUnreachable:
				rep.Unreachable++
			}
		}
		if len(page) < repairPage {
			return rep, nil
		}
		after = page[len(page)-1].Uid
	}
}

// StartCallTranscriptRepair runs the repair in the background, once per
// start, retrying with a growing wait while Dgraph isn't answering.
func StartCallTranscriptRepair(ctx context.Context) {
	helpers.GoSafeNamed("calls.transcript-repair", func() {
		wait := time.Minute
		for try := 0; try < 6; try++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			rep, err := RepairCallTranscripts(ctx, time.Now())
			if err == nil {
				if rep.Ended+rep.Unreachable > 0 {
					helpers.MessageLogs.InfoLog.Printf("Call transcript repair: %s", rep)
				}
				return
			}
			helpers.MessageLogs.ErrorLog.Printf("Call transcript repair failed (will retry): %v (so far: %s)", err, rep)
			wait *= 2
		}
	})
}
