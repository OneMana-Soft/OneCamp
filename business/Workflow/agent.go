package business

// AI in rules (package 9 R3). Two hooks, each set by an AI-edition package
// from its init, so this engine compiles the same with no AI in the build:
//
//   - "Ask an agent" (ask_agent): a step that asks the workspace's own model
//     about the task, as the rule's owner, and posts the answer on the task as
//     the workspace's assistant. business/AI registers the asker. Without it
//     the step can't be saved ("needs the AI edition"), and a saved one fails
//     with that reason.
//   - The hand-off to an AI teammate: a rule that assigns a task to an
//     agent's principal starts the agent, as an assignment by hand does. The
//     hand-off normally rides the task.assigned event. But writes a rule makes
//     never reach any listener (helpers.WithWorkflowGenerated, the loop guard),
//     so the assign step hands the work over itself, through the one hand-off
//     business/AIAgent registers.

import (
	"context"
	"errors"
	"strings"
	"sync"

	contentCodec "github.com/akashc777/OneCamp/business/ContentCodec"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// ActionAskAgent asks the workspace's model about the task and posts the
// answer on it.
const ActionAskAgent = "ask_agent"

// maxAgentPrompt bounds what a rule asks.
const maxAgentPrompt = 2000

// maxAgentAnswer bounds the answer a rule posts, before it is written as the
// comment's HTML.
const maxAgentAnswer = 3000

// AgentQuestion is what a rule asks about a task: its words, the task as the
// rule's owner sees it, and who is asked.
type AgentQuestion struct {
	Prompt string
	Task   *dgraphStruct.DgraphTask
	// Agent is the AI teammate asked (its principal's user id), or "" for the
	// workspace's own assistant, OneCamp AI.
	Agent string
}

// AgentAnswer is how a question was taken: the assistant's words, to post on
// the task as By, or an AI teammate that took the question on as work of its
// own (Queued, its job) and answers on the task itself, as it does any work,
// or one already at work on the task (Busy), which isn't asked again.
type AgentAnswer struct {
	Text   string
	By     uuid.UUID
	Queued string
	Busy   bool
	// Name is who answers, for the run's history.
	Name string
}

// AgentAsker answers a rule's question as the rule's owner, on the
// workspace's own model, under its rate limit and circuit breaker, or hands
// it to the AI teammate named.
type AgentAsker func(ctx context.Context, owner *userModels.UserInfo, q AgentQuestion) (AgentAnswer, error)

// AgentHandOff starts an AI teammate on a task given to it. data is the
// task.assigned event's payload (taskBusiness.AssignedEventData), so it means
// what an assignment by hand means.
type AgentHandOff func(ctx context.Context, data map[string]interface{})

var (
	agentMu      sync.RWMutex
	agentAsker   AgentAsker
	agentHandOff AgentHandOff
)

// ErrNeedsAI is an "ask an agent" step on a build with no AI in it.
var ErrNeedsAI = errors.New("asking an agent needs OneCamp's AI edition")

// RegisterAgentStep sets the asker for "ask an agent". Called from the AI
// edition's init.
func RegisterAgentStep(a AgentAsker) {
	agentMu.Lock()
	defer agentMu.Unlock()
	agentAsker = a
}

// RegisterAgentHandOff sets the hand-off to an AI teammate. Called from the
// AI edition's init.
func RegisterAgentHandOff(h AgentHandOff) {
	agentMu.Lock()
	defer agentMu.Unlock()
	agentHandOff = h
}

// AgentsAvailable says whether this build answers "ask an agent": the AI
// edition does. The builder offers the step only where it is.
func AgentsAvailable() bool { return asker() != nil }

func asker() AgentAsker {
	agentMu.RLock()
	defer agentMu.RUnlock()
	return agentAsker
}

func handOffer() AgentHandOff {
	agentMu.RLock()
	defer agentMu.RUnlock()
	return agentHandOff
}

// Seams for tests: who answers on the workspace's behalf, whether a person
// is an agent's principal, and the comment the answer is.
var (
	actorOf = userDomain.ActorByUUID
	isAgent = func(ctx context.Context, id string) bool {
		u, err := uuid.Parse(id)
		return err == nil && userDomain.IsAgentPrincipal(ctx, u)
	}
	makeCommentAs = taskWriter.CommentAs
)

// askAgent runs an ask_agent step: what it made (the comment it posted, or
// the AI teammate's job), and what the history says of it. Nothing made with
// words said is a step skipped, saying why.
func (cw *compiledWorkflow) askAgent(ctx context.Context, act WorkflowAction, ev triggerEvent, rc *runCtx) (string, string, error) {
	ask := asker()
	if ask == nil {
		return "", "", ErrNeedsAI
	}
	ans, err := ask(ctx, rc.owner, AgentQuestion{Prompt: applyVars(act.Text, ev.vars), Task: rc.task, Agent: act.AgentID})
	if err != nil {
		return "", "", err
	}
	if ans.Queued != "" {
		// The teammate answers on the task, in its own name, as it does work.
		return ans.Queued, "asked " + ans.Name + ", who answers on the task", nil
	}
	if ans.Busy {
		return "", ans.Name + " was already at work on this task, so it wasn't asked again", nil
	}
	answer := strings.TrimSpace(ans.Text)
	if answer == "" {
		return "", "", nil
	}
	if len(answer) > maxAgentAnswer {
		answer = truncate(answer, maxAgentAnswer)
	}
	doc, err := contentCodec.FromMarkdown(answer, nil)
	if err != nil {
		return "", "", err
	}
	by, err := actorOf(ctx, ans.By)
	if err != nil {
		return "", "", err
	}
	id, err := makeCommentAs(ctx, rc.owner, by, rc.taskID(), contentCodec.ToHTML(doc))
	if err != nil {
		return "", "", err
	}
	return id, firstLine(answer, 140), nil
}

// handOffIfAgent starts an AI teammate a rule just gave the task to, and
// says whether it did. A person isn't one, and a build with no AI has no
// hand-off.
func (cw *compiledWorkflow) handOffIfAgent(ctx context.Context, rc *runCtx, to string) bool {
	h := handOffer()
	if h == nil || to == "" {
		return false
	}
	agent, known := false, false
	if rc.shared != nil && rc.shared.agents != nil {
		agent, known = rc.shared.agents[to]
	}
	if !known {
		agent = isAgent(ctx, to)
		if rc.shared != nil {
			if rc.shared.agents == nil {
				rc.shared.agents = map[string]bool{}
			}
			rc.shared.agents[to] = agent
		}
	}
	tid, err := uuid.Parse(rc.task.Uuid)
	if !agent || err != nil {
		return false
	}
	h(ctx, taskBusiness.AssignedEventData(tid, rc.task, to, &rc.owner.UserDgraphInfo))
	return true
}

// firstLine is the first line of a Markdown answer as plain words, at most n
// bytes: what a run's history keeps of it.
func firstLine(md string, n int) string {
	md = strings.TrimSpace(md)
	if i := strings.IndexByte(md, '\n'); i >= 0 {
		md = md[:i]
	}
	line := md
	if doc, err := contentCodec.FromMarkdown(md, nil); err == nil {
		line = contentCodec.PlainText(doc)
	}
	return truncate(strings.TrimSpace(line), n)
}
