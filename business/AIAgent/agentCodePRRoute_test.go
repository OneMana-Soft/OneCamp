package business

import "testing"

// isGitHubContentWriteTool must match raw GitHub file-content write tools across
// naming styles / server prefixes, and must NOT match reads, comments, issues,
// or the governed code_pr tool itself.
func TestIsGitHubContentWriteTool(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"github_create_or_update_file", true},
		{"mcp-github-create-or-update-file", true},
		{"github_push_files", true},
		{"push_files", true},
		{"github_delete_file", true},
		{"github_update_file", true},

		// Not file-content writes.
		{"github_get_file_contents", false},
		{"github_list_commits", false},
		{"list_commits", false},
		{"github_create_pull_request", false},
		{"github_create_branch", false},
		{"github_add_pull_request_review_comment", false},
		{"create_issue", false},
		{"code_pr", false},
		{"send_message", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isGitHubContentWriteTool(tc.name); got != tc.want {
				t.Fatalf("isGitHubContentWriteTool(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestHasGitHubContentWriteTool(t *testing.T) {
	if hasGitHubContentWriteTool(map[string]bool{"github_list_commits": true, "code_pr": true}) {
		t.Fatal("did not expect a write tool among read/code_pr only")
	}
	if !hasGitHubContentWriteTool(map[string]bool{"github_list_commits": true, "github_push_files": true}) {
		t.Fatal("expected push_files to be detected as a content write")
	}
}
