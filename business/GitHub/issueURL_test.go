package business

import "testing"

// An issue or pull request address is read in one place
// (ExtractGitHubIssueInfo), and what it reads is where the workspace's GitHub
// connection is pointed: the owner and repository it names are checked against
// the project's links, then put into the API's path. So it reads only
// addresses GitHub itself writes, and names only what GitHub allows in a name:
// a percent sign, a dot segment or a letter that only folds to ASCII (the
// Kelvin sign is "k" to strings.EqualFold) never passes as part of one.
func TestIssueAddressesAreReadStrictly(t *testing.T) {
	type want struct {
		owner, repo string
		number      int
	}
	good := map[string]want{
		"https://github.com/acme/web/issues/5":                       {"acme", "web", 5},
		"http://github.com/acme/web/issues/5":                        {"acme", "web", 5},
		"HTTPS://GitHub.COM/Acme/Web/issues/5":                       {"Acme", "Web", 5},
		"https://www.github.com/acme/web/pull/7":                     {"acme", "web", 7},
		"https://github.com/acme/web/pull/7/files":                   {"acme", "web", 7},
		"https://github.com/acme/web/issues/5/":                      {"acme", "web", 5},
		"https://github.com/acme/web/issues/5#issuecomment-1":        {"acme", "web", 5},
		"https://github.com/acme/web/issues/5?notification=1":        {"acme", "web", 5},
		"https://github.com/acme/web.git/issues/5":                   {"acme", "web", 5},
		"  https://github.com/acme/web/issues/5\t":                   {"acme", "web", 5},
		"https://github.com/acme-co/my.repo_name-2/issues/12":        {"acme-co", "my.repo_name-2", 12},
		"https://github.com/a/b/issues/2147483647":                   {"a", "b", 2147483647},
		"https://github.com/akashc777/OneCamp-fe-test/pull/128":      {"akashc777", "OneCamp-fe-test", 128},
		"https://github.com/acme/web/pull/5/commits/abc123def456789": {"acme", "web", 5},
	}
	for address, w := range good {
		owner, repo, number, err := ExtractGitHubIssueInfo(address)
		if err != nil || owner != w.owner || repo != w.repo || number != w.number {
			t.Errorf("%q read as %q %q %d (%v), want %q %q %d", address, owner, repo, number, err, w.owner, w.repo, w.number)
		}
	}

	bad := []string{
		"",
		"https://github.com/acme/web",
		"https://github.com/acme/web/issues",
		"https://github.com/acme/web/issues/",
		"https://github.com/acme/web/commits/5",
		"https://github.com/acme/web/Issues/5",
		"https://github.com/acme/web/issues/5abc",
		"https://github.com/acme/web/issues/0",
		"https://github.com/acme/web/issues/-3",
		"https://github.com/acme/web/issues/+3",
		"https://github.com/acme/web/issues/0x10",
		"https://github.com/acme/web/issues/99999999999999999999",
		"github.com/acme/web/issues/5",
		"//github.com/acme/web/issues/5",
		"ftp://github.com/acme/web/issues/5",
		"javascript:alert(1)//github.com/acme/web/issues/5",
		"https://evil.example/acme/web/issues/5",
		"https://github.com.evil.example/acme/web/issues/5",
		"https://gist.github.com/acme/web/issues/5",
		"https://api.github.com/repos/acme/web/issues/5",
		"https://github.com:8443/acme/web/issues/5",
		"https://someone@github.com/acme/web/issues/5",
		"https://github.com//web/issues/5",
		"https://github.com/acme//issues/5",
		"https://github.com/-acme/web/issues/5",
		"https://github.com/acme-/web/issues/5x",
		"https://github.com/acme/../issues/5",
		"https://github.com/acme/./issues/5",
		"https://github.com/../web/issues/5",
		"https://github.com/acme/.git/issues/5",
		"https://github.com/acme%2Fweb/x/issues/5",
		"https://github.com/%61cme/web/issues/5",
		"https://github.com/acme/web%2F..%2Fother/issues/5",
		"https://github.com/acme/w%65b/issues/5",
		"https://github.com/Kcme/web/issues/5",
		"https://github.com/acme/weſb/issues/5",
		"https://github.com/acme/we b/issues/5",
		"https://github.com/acme/web/issues/5\nhttps://github.com/other/x/issues/1",
		"https://github.com/acme/web:x/issues/5",
		"https://github.com/acme/web?/issues/5",
		"https://github.com/acme/web#/issues/5",
		// A dot segment anywhere: the address is shown as a link, which a
		// browser resolves somewhere else.
		"https://github.com/acme/web/issues/5/../../../../x/y/z",
		"https://github.com/acme/web/issues/5/./files",
	}
	for _, address := range bad {
		if owner, repo, number, err := ExtractGitHubIssueInfo(address); err == nil {
			t.Errorf("%q read as %q %q %d, want it refused", address, owner, repo, number)
		}
	}
}
