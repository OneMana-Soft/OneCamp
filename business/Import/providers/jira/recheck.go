package jira

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"

	"github.com/google/uuid"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// recheckFields are what a re-check reads of an issue: who it is kept to and
// why (buildSourceTask's people and restrictedBy), and its project.
const recheckFields = "security,reporter,assignee,creator,project,issuetype"

// Recheck reads each issue's people and security level, and its comments'
// visibility, as Jira has them now, with owner's stored connection: one GET
// an issue, and its comments' pages when some are asked about. A 404 is an
// issue deleted, or hidden from owner: Gone, whatever it was before. The rules
// are the import's own: buildSourceTask (restrictedBy) and keptBy.
func (p *Provider) Recheck(ctx context.Context, owner uuid.UUID, tasks []string, comments map[string][]string) (map[string]importProvider.RecheckedTask, error) {
	tok, err := authFor(ctx, owner)
	if err != nil {
		return nil, err
	}
	api, browse, err := sitesFor(ctx, owner)
	if err != nil {
		return nil, err
	}
	return p.recheck(ctx, tok, api, browse, tasks, comments)
}

// recheck is Recheck with the connection read: its Authorization header
// (tok), where it reaches the API (api) and where issues are browsed.
func (p *Provider) recheck(ctx context.Context, tok, api, browse string, tasks []string, comments map[string][]string) (map[string]importProvider.RecheckedTask, error) {
	out := make(map[string]importProvider.RecheckedTask, len(tasks))
	for _, key := range tasks {
		var iss jiraIssue
		err := p.getJSON(ctx, tok, api+"/rest/api/3/issue/"+url.PathEscape(key)+"?fields="+recheckFields, &iss)
		var gone *importProvider.StatusError
		if errors.As(err, &gone) && gone.Code == http.StatusNotFound {
			out[key] = importProvider.RecheckedTask{Gone: true}
			continue
		}
		if err != nil {
			return nil, err
		}
		got := importProvider.RecheckedTask{Task: buildSourceTask(&iss, "", browse, siteFields{})}
		if asked := comments[key]; len(asked) > 0 {
			if got.KeptComments, got.MissingComments, err = p.commentsKept(ctx, tok, api, key, asked); err != nil {
				return nil, err
			}
		}
		out[key] = got
	}
	return out, nil
}

// commentsKept answers, of an issue's comments asked about (ids), why Jira
// keeps each kept one from some of the people who see the issue (keptBy, read
// with the comments' properties, where a service desk keeps "internal"), and
// those Jira no longer has.
func (p *Provider) commentsKept(ctx context.Context, tok, api, key string, asked []string) (map[string]string, []string, error) {
	left := make(map[string]bool, len(asked))
	for _, id := range asked {
		left[id] = true
	}
	kept := map[string]string{}
	err := p.commentPages(ctx, tok, api, key, "properties", func(c jiraComment) bool {
		if left[c.ID] {
			delete(left, c.ID)
			if why := c.keptBy(); why != "" {
				kept[c.ID] = why
			}
		}
		return len(left) > 0
	})
	if err != nil {
		return nil, nil, err
	}
	missing := make([]string, 0, len(left))
	for id := range left {
		missing = append(missing, id)
	}
	slices.Sort(missing)
	return kept, missing, nil
}
