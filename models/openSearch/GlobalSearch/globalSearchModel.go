package models

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// SearchAnswer is one search's page of hits, how many entries matched in each
// index (the "kinds" aggregation, which a post_filter leaves whole), and how
// many in all.
type SearchAnswer struct {
	Hits    []*openSearchStruct.GlobalSearchOpenSearchResp
	ByIndex map[string]int
	Total   int
}

type searchHit struct {
	opensearchapi.SearchHit
	Highlight map[string][]string `json:"highlight"`
}

type searchResp struct {
	Hits struct {
		Total struct {
			Value int `json:"value"`
		} `json:"total"`
		Hits []searchHit `json:"hits"`
	} `json:"hits"`
	Aggregations struct {
		Kinds struct {
			Buckets []struct {
				Key      string `json:"key"`
				DocCount int    `json:"doc_count"`
			} `json:"buckets"`
		} `json:"kinds"`
	} `json:"aggregations"`
}

// SearchPage runs a search body (business/GlobalSearch's engine builds it)
// over indices: its hits, each read into its type's shape, and the counts.
func SearchPage(ctx context.Context, indices []string, body []byte) (SearchAnswer, error) {
	var out SearchAnswer
	if opensearchInit.OpenSearchClient == nil {
		return out, fmt.Errorf("search is not connected")
	}
	var resp searchResp
	if _, err := opensearchInit.OpenSearchClient.Client.Do(ctx, opensearchapi.SearchReq{
		Indices: indices,
		Body:    bytes.NewReader(body),
	}, &resp); err != nil {
		return out, fmt.Errorf("search: %w", err)
	}
	out.Total = resp.Hits.Total.Value
	out.ByIndex = map[string]int{}
	for _, b := range resp.Aggregations.Kinds.Buckets {
		out.ByIndex[b.Key] = b.DocCount
	}
	for _, hit := range resp.Hits.Hits {
		h, err := decodeHit(hit.Index, hit.Source)
		if err != nil {
			return SearchAnswer{}, fmt.Errorf("search: an entry in %s can't be read: %w", hit.Index, err)
		}
		if h == nil {
			continue
		}
		h.Highlight = hit.Highlight
		h.Sort = hit.Sort
		out.Hits = append(out.Hits, h)
	}
	return out, nil
}

// decodeHit is an entry of an index read into its type's shape; nil for an
// index search doesn't read.
func decodeHit(index string, source json.RawMessage) (*openSearchStruct.GlobalSearchOpenSearchResp, error) {
	h := &openSearchStruct.GlobalSearchOpenSearchResp{}
	var err error
	switch index {
	case openSearchStruct.CHAT_INDEX:
		h.Type, h.Chat = openSearchStruct.CHAT_TYPE, &openSearchStruct.OpenSearchChat{}
		err = json.Unmarshal(source, h.Chat)
	case openSearchStruct.POST_INDEX:
		h.Type, h.Post = openSearchStruct.POST_TYPE, &openSearchStruct.OpenSearchPost{}
		err = json.Unmarshal(source, h.Post)
	case openSearchStruct.COMMENT_INDEX:
		h.Type, h.Comment = openSearchStruct.COMMENT_TYPE, &openSearchStruct.OpenSearchComment{}
		err = json.Unmarshal(source, h.Comment)
	case openSearchStruct.ATTACHMENT_INDEX:
		h.Type, h.Attachment = openSearchStruct.ATTACHMENT_TYPE, &openSearchStruct.OpenSearchAttachment{}
		err = json.Unmarshal(source, h.Attachment)
	case openSearchStruct.DOC_INDEX:
		h.Type, h.Doc = openSearchStruct.DOC_TYPE, &openSearchStruct.OpenSearchDoc{}
		err = json.Unmarshal(source, h.Doc)
	case openSearchStruct.BOARD_INDEX:
		h.Type, h.Board = openSearchStruct.BOARD_TYPE, &openSearchStruct.OpenSearchBoard{}
		err = json.Unmarshal(source, h.Board)
	case openSearchStruct.EVENT_INDEX:
		h.Type, h.Event = openSearchStruct.EVENT_TYPE, &openSearchStruct.OpenSearchEvent{}
		err = json.Unmarshal(source, h.Event)
	case openSearchStruct.TASK_INDEX:
		h.Type, h.Task = openSearchStruct.TASK_TYPE, &openSearchStruct.OpenSearchTask{}
		err = json.Unmarshal(source, h.Task)
	case openSearchStruct.USER_INDEX:
		h.Type, h.User = openSearchStruct.USER_TYPE, &openSearchStruct.OpenSearchUser{}
		err = json.Unmarshal(source, h.User)
	case openSearchStruct.PROJECT_INDEX:
		h.Type, h.Project = openSearchStruct.PROJECT_TYPE, &openSearchStruct.OpenSearchProject{}
		err = json.Unmarshal(source, h.Project)
	case openSearchStruct.CHANNEL_INDEX:
		h.Type, h.Channel = openSearchStruct.CHANNEL_TYPE, &openSearchStruct.OpenSearchChannel{}
		err = json.Unmarshal(source, h.Channel)
	case openSearchStruct.TEAM_INDEX:
		h.Type, h.Team = openSearchStruct.TEAM_TYPE, &openSearchStruct.OpenSearchTeam{}
		err = json.Unmarshal(source, h.Team)
	default:
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return h, nil
}
