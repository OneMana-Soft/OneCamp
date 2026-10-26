package linear

// An issue's comments, files and blocking relations come with the issue as a
// first page of each, and the rest a page at a time from the issue itself.
// The whole of them in one issues query cost about 11,000 of Linear's
// points against its cap of 10,000 (schema_test.go counts every query), and
// comments past the 50th were dropped (22 Oct 2026).

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

type rawComment struct {
	ID        string    `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	User      *struct {
		ID string `json:"id"`
	} `json:"user"`
}

type rawAttachment struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Subtitle string `json:"subtitle"`
}

type rawRelation struct {
	Type  string `json:"type"`
	Issue *struct {
		ID string `json:"id"`
	} `json:"issue"`
}

func (c rawComment) comment() linearComment {
	lc := linearComment{ID: c.ID, Body: c.Body, CreatedAt: c.CreatedAt}
	if c.User != nil {
		lc.UserID = c.User.ID
	}
	return lc
}

// addAttachment keeps a file that has an address, named by its title, else
// its subtitle.
func (iss *linearIssue) addAttachment(a rawAttachment) {
	if a.URL == "" {
		return
	}
	name := a.Title
	if name == "" {
		name = a.Subtitle
	}
	if name == "" {
		name = "attachment"
	}
	iss.Attachments = append(iss.Attachments, linearAttachment{ID: a.ID, Name: name, URL: a.URL})
}

// addRelation keeps an inverse "blocks" relation: an issue that blocks this one.
func (iss *linearIssue) addRelation(rel rawRelation) {
	if rel.Type == "blocks" && rel.Issue != nil && rel.Issue.ID != "" {
		iss.BlockedBy = append(iss.BlockedBy, rel.Issue.ID)
	}
}

const issueCommentsQuery = `
query ($id: String!, $cursor: String) {
  issue(id: $id) {
    comments(first: 100, after: $cursor) {
      nodes { id body createdAt user { id } }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

const issueAttachmentsQuery = `
query ($id: String!, $cursor: String) {
  issue(id: $id) {
    attachments(first: 100, after: $cursor) {
      nodes { id title url subtitle }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

const issueRelationsQuery = `
query ($id: String!, $cursor: String) {
  issue(id: $id) {
    inverseRelations(first: 100, after: $cursor) {
      nodes { type issue { id } }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

// restOfIssue reads the pages after the first of an issue's comments, files
// and relations, for each list that has more.
func (p *Provider) restOfIssue(ctx context.Context, tok string, iss *linearIssue, comments, files, relations pageInfo) error {
	if comments.HasNextPage {
		if err := p.issuePages(ctx, tok, iss.ID, issueCommentsQuery, "comments", comments.EndCursor, func(raw json.RawMessage) error {
			var c rawComment
			if err := json.Unmarshal(raw, &c); err != nil {
				return err
			}
			iss.Comments = append(iss.Comments, c.comment())
			return nil
		}); err != nil {
			return err
		}
	}
	if files.HasNextPage {
		if err := p.issuePages(ctx, tok, iss.ID, issueAttachmentsQuery, "attachments", files.EndCursor, func(raw json.RawMessage) error {
			var a rawAttachment
			if err := json.Unmarshal(raw, &a); err != nil {
				return err
			}
			iss.addAttachment(a)
			return nil
		}); err != nil {
			return err
		}
	}
	if relations.HasNextPage {
		if err := p.issuePages(ctx, tok, iss.ID, issueRelationsQuery, "inverseRelations", relations.EndCursor, func(raw json.RawMessage) error {
			var rel rawRelation
			if err := json.Unmarshal(raw, &rel); err != nil {
				return err
			}
			iss.addRelation(rel)
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// issuePages hands each node of one of an issue's lists, after the cursor,
// to each, a page at a time.
func (p *Provider) issuePages(ctx context.Context, tok, issueID, query, list, after string, each func(json.RawMessage) error) error {
	for page := 0; page < maxPages && after != ""; page++ {
		var resp struct {
			Issue map[string]struct {
				Nodes []json.RawMessage `json:"nodes"`
				Page  pageInfo          `json:"pageInfo"`
			} `json:"issue"`
		}
		if err := p.gql(ctx, tok, query, map[string]any{"id": issueID, "cursor": after}, &resp); err != nil {
			return err
		}
		conn := resp.Issue[list]
		for _, n := range conn.Nodes {
			if err := each(n); err != nil {
				return err
			}
		}
		if !conn.Page.HasNextPage {
			break
		}
		after = conn.Page.EndCursor
	}
	return nil
}

// rateLimited is the wait an answer's body asks for when Linear refused the
// request for its rate limit (HTTP 400 with the code RATELIMITED), nil for
// any other refusal.
func rateLimited(h http.Header, body []byte) *importProvider.ErrRateLimited {
	var env struct {
		Errors []gqlError `json:"errors"`
	}
	if json.Unmarshal(body, &env) != nil {
		return nil
	}
	return limitOf(h, env.Errors)
}

// limitOf is the wait when one of errs is Linear's RATELIMITED: until the
// later of the request and complexity windows resets (the headers give epoch
// milliseconds), or a minute when neither says.
func limitOf(h http.Header, errs []gqlError) *importProvider.ErrRateLimited {
	limited := false
	for _, e := range errs {
		if code, _ := e.Extensions["code"].(string); code == "RATELIMITED" {
			limited = true
		}
	}
	if !limited {
		return nil
	}
	wait := time.Duration(0)
	for _, name := range []string{"X-RateLimit-Requests-Reset", "X-RateLimit-Complexity-Reset", "X-RateLimit-Endpoint-Requests-Reset"} {
		if ms, err := strconv.ParseInt(h.Get(name), 10, 64); err == nil && ms > 0 {
			if d := time.Until(time.UnixMilli(ms)); d > wait {
				wait = d
			}
		}
	}
	if wait <= 0 {
		wait = time.Minute
	}
	if wait > time.Hour {
		wait = time.Hour
	}
	return &importProvider.ErrRateLimited{RetryAfter: wait, Reason: "linear rate limit"}
}
