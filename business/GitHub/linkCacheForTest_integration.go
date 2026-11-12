//go:build integration

package business

import "github.com/akashc777/OneCamp/tests/integration/processcache"

// Every integration test starts with the repositories' links forgotten
// (tests/integration/processcache), as a new process would.
func init() {
	processcache.Register("business/GitHub: the projects each repository is linked to", ForgetLinksForTest)
}

// ForgetLinksForTest makes the process read again which projects link each
// repository, after an integration test starts fresh databases. It exists only
// in the integration build: the tag keeps it out of the shipped binary, so no
// product code can reach it.
//
// The link cache (githubLinkCache.go) keeps a repository's links by its name,
// process-wide, and only link and unlink forget it. Tests write their links
// straight to the table, and reuse names (acme/web): the projects an earlier
// test's databases linked to a repository answered for the next test's, so
// the AI's code tools, held to the projects linking a repository, refused the
// next test's members their own project's repository.
func ForgetLinksForTest() {
	invalidateGitHubLinkCache()
}
