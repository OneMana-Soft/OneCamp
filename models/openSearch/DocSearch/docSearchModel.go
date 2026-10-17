package models

// Runs the docs home's search (domain/DocHome builds it) on the docs index.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// DocHit is one doc a search found, best first, with the passages it was
// found in (escaped text, the words in <mark>).
type DocHit struct {
	UUID         string
	Title        string
	Snippet      string
	BodyPassages []string
	TitlePassage string
}

type docHitSource struct {
	Uuid    string `json:"doc_uuid"`
	Title   string `json:"doc_title"`
	Snippet string `json:"doc_snippet"`
}

type docSearchResp struct {
	Hits struct {
		Total struct {
			Value int `json:"value"`
		} `json:"total"`
		Hits []struct {
			ID        string              `json:"_id"`
			Source    json.RawMessage     `json:"_source"`
			Highlight map[string][]string `json:"highlight"`
		} `json:"hits"`
	} `json:"hits"`
}

// SearchDocs runs a search and answers its hits, best first, and how many it
// counted in all (up to what the query asked to count).
func SearchDocs(ctx context.Context, body string) ([]DocHit, int, error) {
	if opensearchInit.OpenSearchClient == nil {
		return nil, 0, fmt.Errorf("search is not connected")
	}
	var resp docSearchResp
	_, err := opensearchInit.OpenSearchClient.Client.Do(ctx, opensearchapi.SearchReq{
		Indices: []string{openSearchStruct.DOC_INDEX},
		Body:    strings.NewReader(body),
	}, &resp)
	if err != nil {
		return nil, 0, err
	}
	hits := make([]DocHit, 0, len(resp.Hits.Hits))
	for _, h := range resp.Hits.Hits {
		var src docHitSource
		hit := DocHit{UUID: h.ID}
		if err := json.Unmarshal(h.Source, &src); err == nil {
			if src.Uuid != "" {
				hit.UUID = src.Uuid
			}
			hit.Title, hit.Snippet = src.Title, src.Snippet
		}
		hit.BodyPassages = h.Highlight["doc_body"]
		if t := h.Highlight["doc_title"]; len(t) > 0 {
			hit.TitlePassage = t[0]
		}
		hits = append(hits, hit)
	}
	return hits, resp.Hits.Total.Value, nil
}
