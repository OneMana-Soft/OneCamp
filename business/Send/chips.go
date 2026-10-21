package business

import (
	"html"
	"regexp"
	"strings"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A person's mention chip as the composer writes it: its data-id names them
// by "<uuid>@<graph uid>", and its label and words are the sender's.
var personChip = regexp.MustCompile(`<span\b[^>]*\bdata-id="[^"]*@(0x[0-9a-fA-F]+)"[^>]*>[^<]*</span>`)
var (
	chipID    = regexp.MustCompile(`\bdata-id="[^"]*"`)
	chipLabel = regexp.MustCompile(`\bdata-label="[^"]*"`)
	chipWords = regexp.MustCompile(`>[^<]*</span>$`)
)

// normaliseChips is html with each person's chip saying who it notifies (MC-91,
// MC-BUG-7): the uuid and name the graph holds for the uid it names, never the
// sender's. The uid decides who is told, so the words now say the same; a chip
// whose uid names nobody of people is left as it is (resolveMentions refuses
// it before here). Pure.
func normaliseChips(text string, people []*dgraphStruct.DgraphUser) string {
	if len(people) == 0 || !strings.Contains(text, `data-type="mention"`) {
		return text
	}
	byUID := make(map[string]*dgraphStruct.DgraphUser, len(people))
	for _, p := range people {
		if p != nil && p.Uid != "" {
			byUID[strings.ToLower(p.Uid)] = p
		}
	}
	return personChip.ReplaceAllStringFunc(text, func(chip string) string {
		uid := strings.ToLower(personChip.FindStringSubmatch(chip)[1])
		p := byUID[uid]
		if p == nil || p.Uuid == "" {
			return chip
		}
		name := p.DisplayName()
		if name == "" {
			return chip
		}
		label := html.EscapeString(name)
		chip = chipID.ReplaceAllString(chip, `data-id="`+p.Uuid+`@`+uid+`"`)
		if chipLabel.MatchString(chip) {
			chip = chipLabel.ReplaceAllString(chip, `data-label="`+label+`"`)
		}
		return chipWords.ReplaceAllString(chip, ">@"+label+"</span>")
	})
}
