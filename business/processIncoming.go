package business

import (
	"context"
	"fmt"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	webhookModel "github.com/akashc777/OneCamp/models/postgres/Webhook"
	"github.com/google/uuid"
)

// ProcessIncomingDM creates a DM message for an incoming webhook.
// Permission model: the webhook acts with the creator's DM rights.
// We verify the creator exists and is not the recipient; the actual DM-ability
// is governed by the creator's org/team membership (enforced by chatBusiness.CreateChat).
func ProcessIncomingDM(ctx context.Context, webhook *webhookModel.Webhook, dmUUID uuid.UUID, text string, botName string) (string, error) {
	userDgraphInfo, err := userBusiness.GetDgraphUserInfoByUUID(ctx, webhook.CreatedBy.String())
	if err != nil || userDgraphInfo == nil {
		return "", fmt.Errorf("webhook creator user not found")
	}

	// Verify creator is active (not deleted/external)
	if userDgraphInfo.DeletedAt != nil && !userDgraphInfo.DeletedAt.IsZero() {
		return "", fmt.Errorf("webhook creator is inactive")
	}

	sendToDgraph, err := userBusiness.GetDgraphUserInfoByUUID(ctx, dmUUID.String())
	if err != nil || sendToDgraph == nil {
		return "", fmt.Errorf("recipient user not found")
	}

	// Permission check: webhook creator cannot DM themselves
	if webhook.CreatedBy == dmUUID {
		return "", fmt.Errorf("cannot send DM to yourself")
	}

	userInfo := &userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: webhook.CreatedBy},
		UserDgraphInfo:   *userDgraphInfo,
	}

	formattedText := text
	if botName != "" && botName != userDgraphInfo.UserName {
		formattedText = fmt.Sprintf("<p><strong>[%s]</strong> %s</p>", helpers.EscapeHTML(botName), helpers.RemoveHTMLTags(text))
	}

	chatInfo := &chatAdapter.ChatInfo{
		TextHtml: formattedText,
	}

	createdChat, err := chatBusiness.CreateChat(ctx, chatInfo, userInfo, sendToDgraph, dmUUID, nil)
	if err != nil {
		return "", err
	}

	return createdChat.Uuid, nil
}

// ProcessIncomingGroupChat creates a group chat message for an incoming webhook.
// groupId is the grouping_id (not a UUID), matching the grpId used elsewhere in chatBusiness.
func ProcessIncomingGroupChat(ctx context.Context, webhook *webhookModel.Webhook, groupId string, text string, botName string) (string, error) {
	userDgraphInfo, err := userBusiness.GetDgraphUserInfoByUUID(ctx, webhook.CreatedBy.String())
	if err != nil || userDgraphInfo == nil {
		return "", fmt.Errorf("webhook creator user not found")
	}

	// Verify creator is active (not deleted/external)
	if userDgraphInfo.DeletedAt != nil && !userDgraphInfo.DeletedAt.IsZero() {
		return "", fmt.Errorf("webhook creator is inactive")
	}

	// Permission check: webhook creator must be a participant of the group chat
	dmInfo, err := chatBusiness.GetDgraphDmBasicInfoFromDgraph(ctx, userDgraphInfo.Uid, groupId)
	if err != nil || dmInfo == nil {
		return "", fmt.Errorf("group chat not found")
	}
	if dmInfo.ParticipantIsMember == 0 {
		return "", fmt.Errorf("webhook creator is not a member of this group chat")
	}

	userInfo := &userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: webhook.CreatedBy},
		UserDgraphInfo:   *userDgraphInfo,
	}

	formattedText := text
	if botName != "" && botName != userDgraphInfo.UserName {
		formattedText = fmt.Sprintf("<p><strong>[%s]</strong> %s</p>", helpers.EscapeHTML(botName), helpers.RemoveHTMLTags(text))
	}

	chatInfo := &chatAdapter.ChatInfo{
		TextHtml: formattedText,
		GrpUuid:  groupId,
	}

	createdChat, err := chatBusiness.CreateChatForGroup(ctx, chatInfo, userInfo, nil, nil)
	if err != nil {
		return "", err
	}

	return createdChat.Uuid, nil
}
