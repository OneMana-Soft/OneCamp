package helpers

import (
	"regexp"
	"strings"
)

// KeywordPattern is the one rule for a keyword in a message: the whole word,
// any case, as Slack's channel keywords and its outgoing webhook trigger
// words match. Workflows' keywords, outgoing webhooks' trigger words and a
// person's notification keywords (chat P3) all match through it. Nil for an
// empty keyword.
func KeywordPattern(keyword string) *regexp.Regexp {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil
	}
	return regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(keyword) + `\b`)
}
