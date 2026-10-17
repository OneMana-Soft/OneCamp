package business

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	collab "github.com/akashc777/OneCamp/services/collab"
)

// docKind is proposals on docs. Access is the Access Resolver's (A5 behind the
// same interface); every step runs on the doc's live Yjs copy through the Live
// Doc Writer's applyProposal op (A4's client, collaboration-service
// live/ops/applyProposal.js), which reads the proposal and its pending step back
// from the API's internal route.
type docKind struct{}

func init() { Kinds.Register(accessBusiness.KindDoc, docKind{}) }

func (docKind) Access(ctx context.Context, who accessBusiness.Subject, docUUID string) (accessBusiness.Decision, error) {
	d, err := accessBusiness.Rules.Resolve(ctx, who, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: docUUID})
	if err != nil {
		// No such doc and no access answer alike (I9).
		if errors.Is(err, docBusiness.ErrNotFound) {
			return accessBusiness.Decision{}, ErrNotFound
		}
		return d, err
	}
	return d, nil
}

func (docKind) Run(ctx context.Context, who accessBusiness.Subject, p Proposal) error {
	if p.Pending == nil {
		return invalid("no step to run")
	}
	agent := p.AuthorKind == AuthorAgent
	_, err := collab.Live.ApplyProposal(ctx, p.ResourceUUID, p.ID, p.Pending.HunkIDs, collab.Options{
		By: collab.By{UserUUID: who.UserUUID, AgentUUID: who.AgentUUID},
		// An agent's change, once accepted, is marked as one and can be
		// reverted to the version taken just before it (A4's snapshot).
		AgentEdit:     agent && p.Pending.Mode != ModePrepare,
		SnapshotFirst: agent && p.Pending.Mode != ModePrepare,
		// The agent run it belongs to, so the run's Revert finds it.
		RunID: p.RunID,
	})
	if err == nil {
		return nil
	}
	if errors.Is(err, collab.ErrUnavailable) {
		return ErrUnavailable
	}
	if statusOf409(err) {
		return &ConflictError{HunkIDs: conflictIDs(err, p.Pending.HunkIDs)}
	}
	if statusOf404(err) {
		return ErrNotFound
	}
	return err
}

// opStatus is the status in the Live Doc Writer client's error, which reads
// "live <op>: <status> <msg>" (A4's services/collab OpError).
var opStatus = regexp.MustCompile(`live \w+: (\d{3}) `)

// httpStatusOf is the op's answer, from an error with a StatusCode(), or from
// the client's message.
func httpStatusOf(err error) int {
	var coded interface{ StatusCode() int }
	if errors.As(err, &coded) {
		return coded.StatusCode()
	}
	if m := opStatus.FindStringSubmatch(err.Error()); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

func statusOf409(err error) bool {
	return httpStatusOf(err) == 409 || strings.Contains(err.Error(), `{"conflicts":`)
}

func statusOf404(err error) bool { return httpStatusOf(err) == 404 }

// conflictIDs is the hunks the op said no longer apply: its 409 message is
// {"conflicts":[...]}. When the message cannot be read, every hunk asked for.
func conflictIDs(err error, asked []string) []string {
	msg := err.Error()
	if i := strings.Index(msg, `{"conflicts":`); i >= 0 {
		var body struct {
			Conflicts []string `json:"conflicts"`
		}
		if json.NewDecoder(strings.NewReader(msg[i:])).Decode(&body) == nil && len(body.Conflicts) > 0 {
			return body.Conflicts
		}
	}
	return asked
}
