package router

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Search answers through one query: the unified search, which writes the
// searched text into its OpenSearch request as JSON. Six older routes
// (/search/latestChatsAndComments, latestPostsAndComments and
// latestAttachments, each with a "Before" page) built theirs with the text
// formatted in raw, so a quote in what someone typed closed the string and
// could add to the request; two of them were open to any member. Nothing in
// the app called them, and they were broken besides (one passed a graph uid
// where a uuid belonged, and the DM ones filtered on fields no writer sets).
// They are gone; a new search route is a decision, made here.
func TestSearchServesOnlyTheUnifiedSearch(t *testing.T) {
	route := regexp.MustCompile(`\.(Get|Post|Put|Patch|Delete)\("([^"]+)",\s*globalSearchController\.(\w+)\)`)
	var got []string
	for _, line := range routerSource(t) {
		if m := route.FindStringSubmatch(line); m != nil {
			got = append(got, strings.ToUpper(m[1])+" "+m[2]+" "+m[3])
		}
	}
	sort.Strings(got)
	want := []string{
		"GET /unifiedSearch/{search_text} GetUnifiedGlobalSearch",
		"POST /unifiedSearch GetUnifiedGlobalSearch",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the search routes are:\n  %s\nwant only the unified search:\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}
