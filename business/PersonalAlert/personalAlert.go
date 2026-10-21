// Package business (PersonalAlert) is a person's notification keywords and
// VIPs (chat P3 stage 3; PROGRAM.md B5, TN-53, TN-54 and the VIP part of
// TN-58). A keyword tells its person of a channel message that holds it, as
// a mention would; a VIP's messages and mentions reach their person through
// a pause, focus time and quiet hours. Free on every plan (O8).
package business

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/PersonalAlert"
	"github.com/google/uuid"
)

const (
	// MaxKeywords is how many keywords a person keeps.
	MaxKeywords = 50
	// MaxVIPs is how many VIPs a person keeps (Slack's 300).
	MaxVIPs                = 300
	minKeyword, maxKeyword = 2, 64
)

// Refusal is a change refused, in the words the person reads.
type Refusal struct{ Msg string }

func (r *Refusal) Error() string { return r.Msg }

// Status is a refusal's HTTP status.
func (r *Refusal) Status() int { return http.StatusBadRequest }

// Seams for tests.
var (
	readKeywords  = models.Keywords
	writeKeywords = models.SetKeywords
	readVIPs      = models.VIPs
	writeVIPs     = models.SetVIPs
	keywordsOf    = models.KeywordsOf
	livePeople    = func(ctx context.Context, ids []string) ([]*dgraphStruct.DgraphUser, error) {
		return userLight(ctx, ids)
	}
)

// Keywords is a person's keywords.
func Keywords(ctx context.Context, user uuid.UUID) ([]string, error) { return readKeywords(ctx, user) }

// VIP is one of a person's VIPs as their settings show them.
type VIP struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

// VIPs is a person's VIPs who are still in the workspace, in their order.
func VIPs(ctx context.Context, user uuid.UUID) ([]VIP, error) {
	ids, err := readVIPs(ctx, user)
	if err != nil {
		return nil, err
	}
	return liveVIPs(ctx, ids)
}

// liveVIPs is those of ids who are live people, in order, with their names.
func liveVIPs(ctx context.Context, ids []string) ([]VIP, error) {
	out := []VIP{}
	if len(ids) == 0 {
		return out, nil
	}
	people, err := livePeople(ctx, ids)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, p := range people {
		if p != nil {
			names[strings.ToLower(p.Uuid)] = p.DisplayName()
		}
	}
	for _, id := range ids {
		if name, ok := names[strings.ToLower(id)]; ok {
			out = append(out, VIP{UUID: id, Name: name})
		}
	}
	return out, nil
}

// CleanKeywords is keywords as they are kept: trimmed, each once (any case),
// or the refusal. Pure.
func CleanKeywords(keywords []string) ([]string, error) {
	var out []string
	for _, k := range keywords {
		k = strings.Join(strings.Fields(k), " ")
		if k == "" || slices.ContainsFunc(out, func(x string) bool { return strings.EqualFold(x, k) }) {
			continue
		}
		if n := utf8.RuneCountInString(k); n < minKeyword || n > maxKeyword {
			return nil, &Refusal{Msg: "A keyword is 2 to 64 characters."}
		}
		out = append(out, k)
	}
	if len(out) > MaxKeywords {
		return nil, &Refusal{Msg: "You can keep up to 50 keywords."}
	}
	return out, nil
}

// SetKeywords replaces a person's keywords and answers them as kept.
func SetKeywords(ctx context.Context, user uuid.UUID, keywords []string) ([]string, error) {
	clean, err := CleanKeywords(keywords)
	if err != nil {
		return nil, err
	}
	if err := writeKeywords(ctx, user, clean); err != nil {
		return nil, err
	}
	return clean, nil
}

// SetVIPs replaces a person's VIPs (user uuids) with the live people of them,
// never themself, and answers them as kept.
func SetVIPs(ctx context.Context, user uuid.UUID, vips []string) ([]VIP, error) {
	var ids []string
	for _, v := range vips {
		u, err := uuid.Parse(strings.TrimSpace(v))
		if err != nil || u == user || slices.Contains(ids, u.String()) {
			continue
		}
		ids = append(ids, u.String())
	}
	if len(ids) > MaxVIPs {
		return nil, &Refusal{Msg: "You can keep up to 300 VIPs."}
	}
	kept, err := liveVIPs(ctx, ids)
	if err != nil {
		return nil, err
	}
	keep := make([]string, len(kept))
	for i, v := range kept {
		keep[i] = v.UUID
	}
	if err := writeVIPs(ctx, user, keep); err != nil {
		return nil, err
	}
	return kept, nil
}

// KeywordHits is who of people (user ids) a message's words reach by one of
// their keywords: by lower-cased user id, the keyword that matched first.
// text is the message's plain text.
func KeywordHits(ctx context.Context, text string, people []string) (map[string]string, error) {
	hits := map[string]string{}
	if strings.TrimSpace(text) == "" || len(people) == 0 {
		return hits, nil
	}
	kws, err := keywordsOf(ctx, people)
	if err != nil {
		return nil, err
	}
	return matchKeywords(text, kws), nil
}

// matchKeywords is, of each person's keywords, the first text holds. Pure.
func matchKeywords(text string, kws map[string][]string) map[string]string {
	hits := map[string]string{}
	for id, list := range kws {
		for _, k := range list {
			if re := helpers.KeywordPattern(k); re != nil && re.MatchString(text) {
				hits[id] = k
				break
			}
		}
	}
	return hits
}

// IsRefusal is whether err is a rule's refusal.
func IsRefusal(err error) (*Refusal, bool) {
	var r *Refusal
	ok := errors.As(err, &r)
	return r, ok
}

// userLight is the live people ids name (domain/User's light read).
var userLight = func(ctx context.Context, ids []string) ([]*dgraphStruct.DgraphUser, error) {
	return userDomain.GetActiveDgraphUsersByUUIDsLight(ctx, ids)
}
