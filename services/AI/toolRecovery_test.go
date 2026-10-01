package ai

import "testing"

// The exact failed_generation shapes observed from llama-3.3-70b via Groq.
func TestParseTagFunctionCall(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantName string
		wantArgs string
		wantOK   bool
	}{
		{
			"name then json then close tag",
			`<function=mcp_github_list_commits{"owner": "akashc777", "repo": "OneCamp-fe", "since": "2026-07-04T00:00:00Z"}</function>`,
			"mcp_github_list_commits",
			`{"owner": "akashc777", "repo": "OneCamp-fe", "since": "2026-07-04T00:00:00Z"}`,
			true,
		},
		{
			"name with parenthesized json",
			`<function=mcp_github_list_commits({"owner": "akashc777", "repo": "OneCamp-fe", "since": "2026-07-04T00:00:00Z"})>`,
			"mcp_github_list_commits",
			`{"owner": "akashc777", "repo": "OneCamp-fe", "since": "2026-07-04T00:00:00Z"}`,
			true,
		},
		{
			"name close then json",
			`<function=mcp_github_list_commits>{"owner": "akashc777", "repo": "OneCamp-fe"}</function>`,
			"mcp_github_list_commits",
			`{"owner": "akashc777", "repo": "OneCamp-fe"}`,
			true,
		},
		{
			"no args -> empty object",
			`<function=list_my_tasks></function>`,
			"list_my_tasks",
			"{}",
			true,
		},
		{
			"brace inside a string value doesn't close early",
			`<function=send_message{"text": "use {curly} braces"}>`,
			"send_message",
			`{"text": "use {curly} braces"}`,
			true,
		},
		{"no function tag", `just a normal answer`, "", "", false},
		{"empty name", `<function=>{}`, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			name, args, ok := parseTagFunctionCall(c.in)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if name != c.wantName {
				t.Errorf("name = %q, want %q", name, c.wantName)
			}
			if args != c.wantArgs {
				t.Errorf("args = %q, want %q", args, c.wantArgs)
			}
		})
	}
}

func TestRecoverToolCallFromFailedGeneration(t *testing.T) {
	allowed := map[string]bool{"mcp_github_list_commits": true}

	// Real Groq 400 body shape.
	body := []byte(`{"error":{"message":"Failed to call a function. Please adjust your prompt.","type":"invalid_request_error","code":"tool_use_failed","failed_generation":"<function=mcp_github_list_commits{\"owner\": \"akashc777\", \"repo\": \"OneCamp-fe\"}</function>"}}`)
	tc, ok := recoverToolCallFromFailedGeneration(body, allowed)
	if !ok {
		t.Fatal("expected recovery to succeed")
	}
	if tc.Name != "mcp_github_list_commits" {
		t.Errorf("name = %q", tc.Name)
	}
	if tc.Arguments != `{"owner": "akashc777", "repo": "OneCamp-fe"}` {
		t.Errorf("args = %q", tc.Arguments)
	}

	// A tool the run did NOT offer must never be recovered (no invented tools).
	if _, ok := recoverToolCallFromFailedGeneration(body, map[string]bool{"something_else": true}); ok {
		t.Error("must not recover a tool that wasn't offered")
	}

	// A non-tool_use_failed error is left alone.
	other := []byte(`{"error":{"code":"rate_limit_exceeded","failed_generation":""}}`)
	if _, ok := recoverToolCallFromFailedGeneration(other, allowed); ok {
		t.Error("must not recover from a non-tool_use_failed error")
	}

	// Garbage body is safe.
	if _, ok := recoverToolCallFromFailedGeneration([]byte("not json"), allowed); ok {
		t.Error("garbage body must not recover")
	}
}
