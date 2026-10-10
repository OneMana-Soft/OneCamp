package business

import (
	"context"
	"time"

	domain "github.com/akashc777/OneCamp/domain/GlobalSearch"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
)

type GlobalSearchPagination struct {
	Page    []*openSearchStruct.GlobalSearchOpenSearchResp `json:"page,omitempty"`
	HasMore bool                                           `json:"has_more"`
}

func GetLatestChatsAndCommentsFromOpenSearch(ctx context.Context, userUUID string, searchText string) (chatsAndCommentsPage GlobalSearchPagination, err error) {
	chatsAndCommentsInfo, err := domain.GetLatestChatsAndCommentsFromOpenSearch(ctx, userUUID, searchText)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetLatestChatsAndCommentsFromOpenSearch Failed to get chats and comments from opensearch err: %+v",
			err)

		return
	}

	chatsAndCommentsPage = page(ctx, chatsAndCommentsInfo, domain.CHAT_AND_COMMENT_COUNT)

	return
}

func GetLatestChatsAndCommentsFromOpenSearchBeforeTime(ctx context.Context, userUUID string, searchText string, afterTime time.Time) (chatsAndCommentsPage GlobalSearchPagination, err error) {
	chatsAndCommentsInfo, err := domain.GetLatestChatsAndCommentsFromOpenSearchBeforeTime(ctx, userUUID, afterTime, searchText)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetLatestChatsAndCommentsFromOpenSearchBeforeTime Failed to get chats and comments from opensearch err: %+v",
			err)

		return
	}

	chatsAndCommentsPage = page(ctx, chatsAndCommentsInfo, domain.CHAT_AND_COMMENT_COUNT)

	return
}

func GetLatestPostsAndCommentsFromOpenSearch(ctx context.Context, userChannels []*dgraphStruct.DgraphChannel, searchText string) (postsAndCommentsPage GlobalSearchPagination, err error) {
	var channelUUIDs []string

	for _, channel := range userChannels {
		channelUUIDs = append(channelUUIDs, channel.Uuid)
	}
	postsAndCommentsInfo, err := domain.GetLatestPostsAndCommentsFromOpenSearch(ctx, channelUUIDs, searchText)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetLatestPostsAndCommentsFromOpenSearch Failed to get posts and comments from opensearch err: %+v",
			err)

		return
	}

	postsAndCommentsPage = page(ctx, postsAndCommentsInfo, domain.POST_AND_COMMENT_COUNT)

	return
}

func GetLatestPostsAndCommentsFromOpenSearchBeforeTime(ctx context.Context, userChannels []*dgraphStruct.DgraphChannel, searchText string, afterTime time.Time) (postsAndCommentsPage GlobalSearchPagination, err error) {
	var channelUUIDs []string

	for _, channel := range userChannels {
		channelUUIDs = append(channelUUIDs, channel.Uuid)
	}
	postsAndCommentsInfo, err := domain.GetLatestPostsAndCommentsFromOpenSearchBeforeTime(ctx, channelUUIDs, afterTime, searchText)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetLatestPostsAndCommentsFromOpenSearchBeforeTime Failed to get posts and comments from opensearch err: %+v",
			err)

		return
	}

	postsAndCommentsPage = page(ctx, postsAndCommentsInfo, domain.POST_AND_COMMENT_COUNT)

	return
}

func GetLatestAttachmentsFromOpenSearch(ctx context.Context, userUUID string, userChannels []*dgraphStruct.DgraphChannel, searchText string) (attachmentsPage GlobalSearchPagination, err error) {
	var channelUUIDs []string

	for _, channel := range userChannels {
		channelUUIDs = append(channelUUIDs, channel.Uuid)
	}
	attachmentsInfo, err := domain.GetLatestAttachmentsFromOpenSearch(ctx, userUUID, channelUUIDs, searchText)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetLatestAttachmentsFromOpenSearch Failed to get attachments from opensearch err: %+v",
			err)

		return
	}

	attachmentsPage = page(ctx, attachmentsInfo, domain.ATTACHMENT_COUNT)

	return
}

func GetLatestAttachmentsFromOpenSearchBeforeTime(ctx context.Context, userUUID string, userChannels []*dgraphStruct.DgraphChannel, searchText string, afterTime time.Time) (attachmentsPage GlobalSearchPagination, err error) {
	var channelUUIDs []string

	for _, channel := range userChannels {
		channelUUIDs = append(channelUUIDs, channel.Uuid)
	}
	attachmentsInfo, err := domain.GetLatestAttachmentsFromOpenSearchBeforeTime(ctx, userUUID, channelUUIDs, afterTime, searchText)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetLatestAttachmentsFromOpenSearchBeforeTime Failed to get attachments from opensearch err: %+v",
			err)

		return
	}

	attachmentsPage = page(ctx, attachmentsInfo, domain.ATTACHMENT_COUNT)

	return
}
func GetUnifiedGlobalSearch(ctx context.Context, userUUID string, userEmail string, userChannels []*dgraphStruct.DgraphChannel, userProjects []*dgraphStruct.DgraphProject, userTeams []*dgraphStruct.DgraphTeam, searchText string) (searchPage GlobalSearchPagination, err error) {
	var channelUUIDs []string
	for _, ch := range userChannels {
		channelUUIDs = append(channelUUIDs, ch.Uuid)
	}

	projectUUIDMap := make(map[string]bool)
	for _, proj := range userProjects {
		projectUUIDMap[proj.Uuid] = true
	}

	for _, team := range userTeams {
		for _, proj := range team.Projects {
			projectUUIDMap[proj.Uuid] = true
		}
	}

	var projectUUIDs []string
	for uuid := range projectUUIDMap {
		projectUUIDs = append(projectUUIDs, uuid)
	}

	var teamUUIDs []string
	for _, team := range userTeams {
		teamUUIDs = append(teamUUIDs, team.Uuid)
	}

	searchInfo, err := domain.GetUnifiedGlobalSearchFromOpenSearch(ctx, userUUID, userEmail, channelUUIDs, projectUUIDs, teamUUIDs, searchText)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetUnifiedGlobalSearch Failed to get unified search from opensearch err: %+v",
			err)

		return
	}

	searchPage = page(ctx, searchInfo, domain.UNIFIED_SEARCH_COUNT)

	return
}
