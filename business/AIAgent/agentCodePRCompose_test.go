package business

import "testing"

func TestCodePRComposeInstruction(t *testing.T) {
	base := "fix the flaky login test"
	cases := []struct {
		name string
		repo string
		want string
	}{
		{"valid slug", "octocat/hello-world", "Target repository: octocat/hello-world.\n\n" + base},
		{"strips .git", "octocat/hello-world.git", "Target repository: octocat/hello-world.\n\n" + base},
		{"trims spaces", "  octocat/hello-world  ", "Target repository: octocat/hello-world.\n\n" + base},
		{"empty ignored", "", base},
		{"url ignored (not exact slug)", "https://github.com/octocat/hello-world", base},
		{"path ignored", "src/app/main.go", base},
		{"space in value ignored", "octo cat/hello", base},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := codePRComposeInstruction(c.repo, base); got != c.want {
				t.Fatalf("codePRComposeInstruction(%q) = %q, want %q", c.repo, got, c.want)
			}
		})
	}
}

func TestCodePRComposeInstruction_NoDuplicateLead(t *testing.T) {
	instr := "Target repository: octocat/hello-world.\n\ndo the thing"
	if got := codePRComposeInstruction("octocat/hello-world", instr); got != instr {
		t.Fatalf("should not duplicate an existing lead line, got %q", got)
	}
}
