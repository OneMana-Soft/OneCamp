package models

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

type customSearchHit struct {
	opensearchapi.SearchHit
	Highlight map[string][]string `json:"highlight"`
}

type customSearchResp struct {
	Hits struct {
		Hits []customSearchHit `json:"hits"`
	} `json:"hits"`
}

func GetUnifiedGlobalSearchFromOpenSearch(ctx context.Context, query string) (searchInfo []*openSearchStruct.GlobalSearchOpenSearchResp, err error) {

	var resp customSearchResp
	_, err = opensearchInit.OpenSearchClient.Client.Do(ctx, opensearchapi.SearchReq{
		Indices: []string{openSearchStruct.CHAT_INDEX, openSearchStruct.POST_INDEX, openSearchStruct.COMMENT_INDEX, openSearchStruct.ATTACHMENT_INDEX, openSearchStruct.DOC_INDEX, openSearchStruct.BOARD_INDEX, openSearchStruct.TASK_INDEX, openSearchStruct.USER_INDEX, openSearchStruct.PROJECT_INDEX, openSearchStruct.CHANNEL_INDEX, openSearchStruct.TEAM_INDEX},
		Body:    strings.NewReader(query),
	}, &resp)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetUnifiedGlobalSearchFromOpenSearch Failed to exec query err: %+v",
			err)
		return
	}

	for _, hit := range resp.Hits.Hits {
		var hitInfo openSearchStruct.GlobalSearchOpenSearchResp
		hitInfo.Highlight = hit.Highlight
		switch hit.Index {
		case openSearchStruct.CHAT_INDEX:
			var chatInfo openSearchStruct.OpenSearchChat
			err = json.Unmarshal(hit.Source, &chatInfo)
			if err != nil {
				helpers.LogErrorWithContext(ctx,
					"models/GetUnifiedGlobalSearchFromOpenSearch Failed to unmarshal open search chat json err: %+v",
					err)
				return
			}
			hitInfo.Type = openSearchStruct.CHAT_TYPE
			hitInfo.Chat = &chatInfo

		case openSearchStruct.POST_INDEX:
			var postInfo openSearchStruct.OpenSearchPost
			err = json.Unmarshal(hit.Source, &postInfo)
			if err != nil {
				helpers.LogErrorWithContext(ctx,
					"models/GetUnifiedGlobalSearchFromOpenSearch Failed to unmarshal open search post json err: %+v",
					err)
				return
			}
			hitInfo.Type = openSearchStruct.POST_TYPE
			hitInfo.Post = &postInfo

		case openSearchStruct.COMMENT_INDEX:
			var commentInfo openSearchStruct.OpenSearchComment
			err = json.Unmarshal(hit.Source, &commentInfo)
			if err != nil {
				helpers.LogErrorWithContext(ctx,
					"models/GetUnifiedGlobalSearchFromOpenSearch Failed to unmarshal open search comment json err: %+v",
					err)
				return
			}
			hitInfo.Type = openSearchStruct.COMMENT_TYPE
			hitInfo.Comment = &commentInfo

		case openSearchStruct.ATTACHMENT_INDEX:
			var attachmentInfo openSearchStruct.OpenSearchAttachment
			err = json.Unmarshal(hit.Source, &attachmentInfo)
			if err != nil {
				helpers.LogErrorWithContext(ctx,
					"models/GetUnifiedGlobalSearchFromOpenSearch Failed to unmarshal open search attachment json err: %+v",
					err)
				return
			}
			hitInfo.Type = openSearchStruct.ATTACHMENT_TYPE
			hitInfo.Attachment = &attachmentInfo

		case openSearchStruct.DOC_INDEX:
			var docInfo openSearchStruct.OpenSearchDoc
			err = json.Unmarshal(hit.Source, &docInfo)
			if err != nil {
				helpers.LogErrorWithContext(ctx,
					"models/GetUnifiedGlobalSearchFromOpenSearch Failed to unmarshal open search doc json err: %+v",
					err)
				return
			}
			hitInfo.Type = openSearchStruct.DOC_TYPE
			hitInfo.Doc = &docInfo

		case openSearchStruct.BOARD_INDEX:
			var boardInfo openSearchStruct.OpenSearchBoard
			err = json.Unmarshal(hit.Source, &boardInfo)
			if err != nil {
				helpers.LogErrorWithContext(ctx,
					"models/GetUnifiedGlobalSearchFromOpenSearch Failed to unmarshal open search board json err: %+v",
					err)
				return
			}
			hitInfo.Type = openSearchStruct.BOARD_TYPE
			hitInfo.Board = &boardInfo

		case openSearchStruct.TASK_INDEX:
			var taskInfo openSearchStruct.OpenSearchTask
			err = json.Unmarshal(hit.Source, &taskInfo)
			if err != nil {
				helpers.LogErrorWithContext(ctx,
					"models/GetUnifiedGlobalSearchFromOpenSearch Failed to unmarshal open search task json err: %+v",
					err)
				return
			}
			hitInfo.Type = openSearchStruct.TASK_TYPE
			hitInfo.Task = &taskInfo

		case openSearchStruct.USER_INDEX:
			var userInfo openSearchStruct.OpenSearchUser
			err = json.Unmarshal(hit.Source, &userInfo)
			if err != nil {
				helpers.LogErrorWithContext(ctx,
					"models/GetUnifiedGlobalSearchFromOpenSearch Failed to unmarshal open search user json err: %+v",
					err)
				return
			}
			hitInfo.Type = openSearchStruct.USER_TYPE
			hitInfo.User = &userInfo

		case openSearchStruct.PROJECT_INDEX:
			var projectInfo openSearchStruct.OpenSearchProject
			err = json.Unmarshal(hit.Source, &projectInfo)
			if err != nil {
				helpers.LogErrorWithContext(ctx,
					"models/GetUnifiedGlobalSearchFromOpenSearch Failed to unmarshal open search project json err: %+v",
					err)
				return
			}
			hitInfo.Type = openSearchStruct.PROJECT_TYPE
			hitInfo.Project = &projectInfo

		case openSearchStruct.CHANNEL_INDEX:
			var channelInfo openSearchStruct.OpenSearchChannel
			err = json.Unmarshal(hit.Source, &channelInfo)
			if err != nil {
				helpers.LogErrorWithContext(ctx,
					"models/GetUnifiedGlobalSearchFromOpenSearch Failed to unmarshal open search channel json err: %+v",
					err)
				return
			}
			hitInfo.Type = openSearchStruct.CHANNEL_TYPE
			hitInfo.Channel = &channelInfo

		case openSearchStruct.TEAM_INDEX:
			var teamInfo openSearchStruct.OpenSearchTeam
			err = json.Unmarshal(hit.Source, &teamInfo)
			if err != nil {
				helpers.LogErrorWithContext(ctx,
					"models/GetUnifiedGlobalSearchFromOpenSearch Failed to unmarshal open search team json err: %+v",
					err)
				return
			}
			hitInfo.Type = openSearchStruct.TEAM_TYPE
			hitInfo.Team = &teamInfo
		}

		searchInfo = append(searchInfo, &hitInfo)
	}

	return
}

func GetChatsAndCommentsFromOpenSearch(ctx context.Context, query string) (chatsAndCommentsInfo []*openSearchStruct.GlobalSearchOpenSearchResp, err error) {

	var resp customSearchResp
	_, err = opensearchInit.OpenSearchClient.Client.Do(ctx, opensearchapi.SearchReq{
		Indices: []string{openSearchStruct.CHAT_INDEX, openSearchStruct.COMMENT_INDEX},
		Body:    strings.NewReader(query),
	}, &resp)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetChatsAndCommentsFromOpenSearch Failed to exec query err: %+v",
			err)
		return
	}

	for _, hit := range resp.Hits.Hits {
		var hitInfo openSearchStruct.GlobalSearchOpenSearchResp
		hitInfo.Highlight = hit.Highlight
		switch hit.Index {
		case openSearchStruct.CHAT_INDEX:
			var chatInfo openSearchStruct.OpenSearchChat
			err = json.Unmarshal(hit.Source, &chatInfo)
			if err != nil {
				helpers.LogErrorWithContext(ctx,
					"models/GetChatsAndCommentsFromOpenSearch Failed to unmarshal open search chat json err: %+v",
					err)
				return
			}

			hitInfo.Type = openSearchStruct.CHAT_TYPE
			hitInfo.Chat = &chatInfo

		case openSearchStruct.COMMENT_INDEX:
			var commentInfo openSearchStruct.OpenSearchComment
			err = json.Unmarshal(hit.Source, &commentInfo)
			if err != nil {
				helpers.LogErrorWithContext(ctx,
					"models/GetChatsAndCommentsFromOpenSearch Failed to unmarshal open search comment json err: %+v",
					err)
				return
			}

			hitInfo.Type = openSearchStruct.COMMENT_TYPE
			hitInfo.Comment = &commentInfo

		}

		chatsAndCommentsInfo = append(chatsAndCommentsInfo, &hitInfo)
	}

	return

}

func GetPostsAndCommentsFromOpenSearch(ctx context.Context, query string) (postsAndCommentsInfo []*openSearchStruct.GlobalSearchOpenSearchResp, err error) {

	var resp customSearchResp
	_, err = opensearchInit.OpenSearchClient.Client.Do(ctx, opensearchapi.SearchReq{
		Indices: []string{openSearchStruct.POST_INDEX, openSearchStruct.COMMENT_INDEX},
		Body:    strings.NewReader(query),
	}, &resp)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetPostsAndCommentsFromOpenSearch Failed to exec query err: %+v",
			err)
		return
	}

	for _, hit := range resp.Hits.Hits {
		var hitInfo openSearchStruct.GlobalSearchOpenSearchResp
		hitInfo.Highlight = hit.Highlight
		switch hit.Index {
		case openSearchStruct.POST_INDEX:
			var postInfo openSearchStruct.OpenSearchPost
			err = json.Unmarshal(hit.Source, &postInfo)
			if err != nil {
				helpers.LogErrorWithContext(ctx,
					"models/GetPostsAndCommentsFromOpenSearch Failed to unmarshal open search post json err: %+v",
					err)
				return
			}

			hitInfo.Type = openSearchStruct.POST_TYPE
			hitInfo.Post = &postInfo

		case openSearchStruct.COMMENT_INDEX:
			var commentInfo openSearchStruct.OpenSearchComment
			err = json.Unmarshal(hit.Source, &commentInfo)
			if err != nil {
				helpers.LogErrorWithContext(ctx,
					"models/GetPostsAndCommentsFromOpenSearch Failed to unmarshal open search comment json err: %+v",
					err)
				return
			}

			hitInfo.Type = openSearchStruct.COMMENT_TYPE
			hitInfo.Comment = &commentInfo

		}

		postsAndCommentsInfo = append(postsAndCommentsInfo, &hitInfo)
	}

	return

}

func GetAttachmentsFromOpenSearch(ctx context.Context, query string) (attachmentsInfo []*openSearchStruct.GlobalSearchOpenSearchResp, err error) {

	var resp customSearchResp
	_, err = opensearchInit.OpenSearchClient.Client.Do(ctx, opensearchapi.SearchReq{
		Indices: []string{openSearchStruct.ATTACHMENT_INDEX},
		Body:    strings.NewReader(query),
	}, &resp)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetAttachmentsFromOpenSearch Failed to exec query err: %+v",
			err)
		return
	}

	for _, hit := range resp.Hits.Hits {
		var hitInfo openSearchStruct.GlobalSearchOpenSearchResp
		hitInfo.Highlight = hit.Highlight

		var attachmentInfo openSearchStruct.OpenSearchAttachment
		err = json.Unmarshal(hit.Source, &attachmentInfo)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/GetAttachmentsFromOpenSearch Failed to unmarshal open search attachment json err: %+v",
				err)
			return
		}

		hitInfo.Type = openSearchStruct.ATTACHMENT_TYPE
		hitInfo.Attachment = &attachmentInfo

		attachmentsInfo = append(attachmentsInfo, &hitInfo)
	}

	return

}
