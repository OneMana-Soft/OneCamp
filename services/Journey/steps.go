package journey

// The first five minutes, as a person actually spends them.
//
// Each step is one thing somebody does, and each says what a pass proves and
// what it does not. Where a step covers ground the admin health page cannot,
// that is called out: the two are complementary, and the value of this one is
// precisely the questions a read-only probe is unable to ask.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

func steps() []step {
	return []step{
		{
			Name: "identity",
			Describe: "The token authenticates and resolves to a user. It does not prove the token has the " +
				"scopes the later steps need; those fail on their own terms if it does not.",
			Run: func(ctx context.Context, c *client, _ *state) error {
				env, err := c.do(ctx, "GET", "/v1/me", nil)
				if err != nil {
					return err
				}
				var me struct {
					Id     string   `json:"id"`
					Scopes []string `json:"scopes"`
				}
				if jerr := json.Unmarshal(env.Data, &me); jerr != nil {
					return fmt.Errorf("could not read the identity response: %w", jerr)
				}
				if me.Id == "" {
					return fmt.Errorf("authenticated but no user id came back, so the token is valid and " +
						"resolves to nobody")
				}
				return nil
			},
		},
		{
			Name: "projects-readable",
			Describe: "Listing projects returns rows. This is the shape of the defect that broke entity links " +
				"for the product's whole life: a filter that matched nothing, so every list came back empty " +
				"with no error. An empty workspace cannot be told apart from a broken filter here, and the " +
				"step says which it saw.",
			Run: func(ctx context.Context, c *client, _ *state) error {
				env, err := c.do(ctx, "GET", "/v1/projects", nil)
				if err != nil {
					return err
				}
				n, cerr := countItems(env.Data)
				if cerr != nil {
					return cerr
				}
				if n == 0 {
					return fmt.Errorf("no projects came back. If this workspace genuinely has none that is " +
						"correct; if it has projects, the membership or soft-delete filter is matching nothing, " +
						"which is how entity links stayed broken from the day they shipped")
				}
				return nil
			},
		},
		{
			Name:       "task-create",
			NeedsWrite: true,
			Describe: "A task can be created through the public API. It does not prove the task is visible to " +
				"anyone else, only that the write was accepted.",
			Run: func(ctx context.Context, c *client, st *state) error {
				env, err := c.do(ctx, "POST", "/v1/tasks", map[string]string{
					"task_name":    "journey check " + st.marker,
					"project_uuid": st.ProjectUUID,
					"description":  "Created by the OneCamp journey check. Safe to delete.",
				})
				if err != nil {
					return err
				}
				st.taskUUID = findUUID(env.Data)
				if st.taskUUID == "" {
					// Not fatal on its own: the readback searches by marker, so
					// a missing id costs the status step, not this one.
					return nil
				}
				return nil
			},
		},
		{
			Name:       "task-readback",
			NeedsWrite: true,
			Describe: "The task just created can be found again. THIS IS THE SEAM the health page cannot " +
				"reach: writing and reading are different code paths over different filters, and every " +
				"silent failure this product has had lived in the gap between them.",
			Run: func(ctx context.Context, c *client, st *state) error {
				env, err := c.do(ctx, "GET", "/v1/tasks?search="+url.QueryEscape(st.marker), nil)
				if err != nil {
					return err
				}
				if !strings.Contains(string(env.Data), st.marker) {
					return fmt.Errorf("the task was created a moment ago and listing tasks does not contain "+
						"it (marker %q). The write was accepted and the read cannot see it", st.marker)
				}
				return nil
			},
		},
		{
			Name:       "task-status",
			NeedsWrite: true,
			Describe: "The task can be moved to done, which is the ordinary update path and the one that " +
				"triggers integration side effects. It does not prove those side effects completed.",
			Run: func(ctx context.Context, c *client, st *state) error {
				if st.taskUUID == "" {
					return fmt.Errorf("no task id came back from creating the task, so its status cannot be " +
						"changed; the create response did not carry an identifier")
				}
				_, err := c.do(ctx, "POST", "/v1/tasks/"+url.PathEscape(st.taskUUID)+"/status",
					map[string]string{"status": "done"})
				return err
			},
		},
		{
			Name:       "search-finds-it",
			NeedsWrite: true,
			Describe: "Search returns the task that was just created. The admin health page checks that the " +
				"search indices EXIST and says plainly that it cannot prove documents reach them. This is " +
				"the step that proves it, and a failure here means search is quietly returning nothing.",
			Run: func(ctx context.Context, c *client, st *state) error {
				env, err := c.do(ctx, "GET", "/v1/search?q="+url.QueryEscape(st.marker), nil)
				if err != nil {
					return err
				}
				if !strings.Contains(string(env.Data), st.marker) {
					return fmt.Errorf("search cannot find the task created moments ago (marker %q). The "+
						"index exists or an earlier step would have failed, so documents are not reaching "+
						"it: to a user this looks like search finding nothing rather than an error", st.marker)
				}
				return nil
			},
		},
	}
}

// countItems counts a JSON array, or the first array inside a JSON object, so
// the steps do not each need to know a handler's exact envelope shape.
func countItems(raw json.RawMessage) (int, error) {
	var asArray []json.RawMessage
	if err := json.Unmarshal(raw, &asArray); err == nil {
		return len(asArray), nil
	}
	var asObject map[string]json.RawMessage
	if err := json.Unmarshal(raw, &asObject); err != nil {
		return 0, fmt.Errorf("the response was neither a list nor an object: %s", snippet(raw))
	}
	for _, v := range asObject {
		var nested []json.RawMessage
		if err := json.Unmarshal(v, &nested); err == nil {
			return len(nested), nil
		}
	}
	return 0, nil
}

// findUUID pulls the first UUID-shaped string value out of a response.
//
// Deliberately structural rather than a typed decode. These handlers delegate to
// the shared tool executors, whose payload shape is theirs to change, and a
// journey check that stops working because a field was renamed is a check that
// gets deleted. The id is only used to change the task's status; the readback
// and the search both match on the marker instead, so a miss here costs one step
// rather than the run.
func findUUID(raw json.RawMessage) string {
	var walk func(v interface{}) string
	walk = func(v interface{}) string {
		switch t := v.(type) {
		case string:
			if looksLikeUUID(t) {
				return t
			}
		case map[string]interface{}:
			// Prefer an explicitly named id before falling back to any UUID.
			for _, key := range []string{"task_uuid", "uuid", "id"} {
				if s, ok := t[key].(string); ok && looksLikeUUID(s) {
					return s
				}
			}
			for _, sub := range t {
				if found := walk(sub); found != "" {
					return found
				}
			}
		case []interface{}:
			for _, sub := range t {
				if found := walk(sub); found != "" {
					return found
				}
			}
		}
		return ""
	}

	var any interface{}
	if err := json.Unmarshal(raw, &any); err != nil {
		return ""
	}
	return walk(any)
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}
