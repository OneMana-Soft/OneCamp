package journey

// Does the PRODUCT work, as opposed to the installation?
//
// WHY THIS EXISTS SEPARATELY FROM THE HEALTH CHECKS. The admin health page
// answers "are the services this install needs reachable, and has a known defect
// come back". It is read-only by contract, which is what makes it safe for an
// admin to press, and that contract is exactly why it cannot answer the question
// that actually matters to a person using OneCamp: can I create a task and then
// find it again.
//
// Every serious defect this product has shipped lived in that gap. Entity links
// were broken from the day they landed because a filter matched nothing; the
// services were all up. The GitHub dialog never opened; the services were all
// up. A demo user clicking their own assigned task got a 403; the services were
// all up. The suite was green throughout, because a unit test calls a function
// and the failures were in the seams between deployed pieces.
//
// So this is a REAL CLIENT. It speaks HTTP to a running instance with a bearer
// token, through the same middleware, serialisation and authorisation a customer
// integration goes through. Nothing here imports the business layer, on purpose:
// calling business code directly would have passed during every one of the
// outages above.
//
// IT WRITES, which is why it is a separate command and not another probe. It
// creates a task, so it needs a project to create it in, and that project is
// named explicitly by the operator rather than discovered. Nobody should press a
// button and find test rows in a real project afterwards.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// outcome is what happened to one step.
type outcome string

const (
	outcomePassed outcome = "passed"
	outcomeFailed outcome = "failed"
	// skipped: the step needs configuration this run does not have. Not a
	// failure, and reported distinctly so an operator is never told a write path
	// works when it was never exercised.
	outcomeSkipped outcome = "skipped"
	// notReached: an earlier step failed and this one depends on it. Reported
	// rather than run, because four cascading failures hide which one was first.
	outcomeNotReached outcome = "not reached"
)

// step is one thing a person does, and what doing it proves.
type step struct {
	Name string
	// Describe says what a pass proves AND what it does not, in the same terms
	// the health checks use. A green tick with no scope is worth less than
	// nothing whichever page it appears on.
	Describe string
	// NeedsWrite marks a step that creates or changes data, so a run with no
	// project configured can skip it honestly instead of failing.
	NeedsWrite bool
	Run        func(ctx context.Context, c *client, st *state) error
}

// state is what one step hands the next. Read-after-write is the seam that keeps
// breaking, so the id of the thing just created has to survive.
type state struct {
	ProjectUUID string
	// marker is unique per run and goes into the task name, so the readback and
	// the search look for THIS run's row and not a previous one's. Without it a
	// leftover task from an earlier run makes a broken readback look healthy.
	marker   string
	taskUUID string
}

type result struct {
	Name     string
	Describe string
	Outcome  outcome
	Detail   string
	TookMs   int64
}

// client is a minimal HTTP client for the public API surface.
type client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// envelope is what every /v1 handler returns: {"message":..,"data":..} on
// success, {"msg":..} on a refusal.
type envelope struct {
	Message string          `json:"message"`
	Msg     string          `json:"msg"`
	Data    json.RawMessage `json:"data"`
}

func (c *client) do(ctx context.Context, method, path string, body interface{}) (*envelope, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding the request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, reader)
	if err != nil {
		return nil, fmt.Errorf("building the request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	// Capped: a handler that returns a page of HTML instead of JSON should not
	// be read into memory whole, and the first part is enough to diagnose it.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s %s: reading the response: %w", method, path, err)
	}

	var env envelope
	// A body that will not parse is itself the finding, so the status and a
	// snippet are reported rather than a JSON error nobody can act on.
	if jerr := json.Unmarshal(raw, &env); jerr != nil {
		return nil, fmt.Errorf("%s %s returned %d with a body that is not JSON: %s",
			method, path, resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode != http.StatusOK {
		// Two different envelopes reach here and both are legitimate: the tool
		// handlers refuse with {"msg":...} and the auth middleware refuses with
		// {"message":...}. Reading only one of them turned a plain "invalid or
		// expired token" into a page of raw JSON, which is exactly the sort of
		// thing that never shows up until the command is pointed at something
		// real.
		reason := helpers.FirstNonBlank(env.Msg, env.Message, snippet(raw))
		return nil, fmt.Errorf("%s %s returned %d: %s", method, path, resp.StatusCode, reason)
	}
	return &env, nil
}

func snippet(raw []byte) string {
	const max = 200
	s := strings.TrimSpace(string(raw))
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// run executes the steps in order, stopping at the first failure.
//
// Stopping is deliberate. The steps depend on each other -- a task cannot be
// read back if it was never created -- so continuing would report four failures
// for one fault and bury which came first.
func run(ctx context.Context, c *client, st *state, steps []step) []result {
	out := make([]result, 0, len(steps))
	stopped := false

	for _, s := range steps {
		if stopped {
			out = append(out, result{Name: s.Name, Describe: s.Describe, Outcome: outcomeNotReached})
			continue
		}
		if s.NeedsWrite && st.ProjectUUID == "" {
			out = append(out, result{
				Name: s.Name, Describe: s.Describe, Outcome: outcomeSkipped,
				Detail: "no project configured, so nothing was written; set ONECAMP_JOURNEY_PROJECT to exercise this",
			})
			continue
		}

		started := time.Now()
		err := s.Run(ctx, c, st)
		r := result{Name: s.Name, Describe: s.Describe, Outcome: outcomePassed, TookMs: time.Since(started).Milliseconds()}
		if err != nil {
			r.Outcome = outcomeFailed
			r.Detail = err.Error()
			stopped = true
		}
		out = append(out, r)
	}
	return out
}
