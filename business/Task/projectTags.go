package business

import (
	"context"
	"sort"
	"strings"

	domain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// TagCount is one of a project's tags and how many live tasks carry it.
type TagCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// CountTags tallies tags across task labels, ignoring case and keeping the
// spelling seen first, most used first. Pure, for its test.
func CountTags(tasks []*dgraphStruct.DgraphTask) []TagCount {
	byKey := map[string]*TagCount{}
	var order []string
	for _, t := range tasks {
		if t == nil || t.Label == nil {
			continue
		}
		for _, tag := range helpers.Tags(*t.Label) {
			k := strings.ToLower(tag)
			if c, ok := byKey[k]; ok {
				c.Count++
				continue
			}
			byKey[k] = &TagCount{Name: tag, Count: 1}
			order = append(order, k)
		}
	}
	out := make([]TagCount, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// ProjectTags is the tags in use in a project, for picking and filtering.
func ProjectTags(ctx context.Context, projectUUID string) ([]TagCount, error) {
	tasks, err := domain.GetDgraphProjectTaskLabels(ctx, projectUUID)
	if err != nil {
		return nil, err
	}
	return CountTags(tasks), nil
}
