package business

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	calendarAdapter "github.com/akashc777/OneCamp/adapter/Calendar"
	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	docAdapter "github.com/akashc777/OneCamp/adapter/Doc"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	taskAdapter "github.com/akashc777/OneCamp/adapter/Task"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	principalBusiness "github.com/akashc777/OneCamp/business/Principal"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	domainUser "github.com/akashc777/OneCamp/domain/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// RegisterToolExecutors wires AI tool definitions to actual business functions.
// Called via init() when this package is imported.
func RegisterToolExecutors() {
	ai.RegisterExecutor("create_task", executeCreateTask)
	ai.RegisterExecutor("update_task_status", executeUpdateTaskStatus)
	ai.RegisterExecutor("assign_task", executeAssignTask)
	ai.RegisterExecutor("set_task_due_date", executeSetTaskDueDate)
	ai.RegisterExecutor("list_tasks", executeListTasks)
	ai.RegisterExecutor("list_project_tasks", executeListProjectTasks)
	ai.RegisterExecutor("list_projects", executeListProjects)
	ai.RegisterExecutor("read_project", executeReadProject)
	ai.RegisterExecutor("list_teams", executeListTeams)
	ai.RegisterExecutor("create_project", executeCreateProject)
	ai.RegisterExecutor("create_doc", executeCreateDoc)
	ai.RegisterExecutor("read_doc", executeReadDoc)
	ai.RegisterExecutor("append_to_doc", executeAppendToDoc)
	ai.RegisterExecutor("find_people", executeFindPeople)
	ai.RegisterExecutor("read_meeting_transcript", executeReadMeetingTranscript)
	ai.RegisterExecutor("send_message", executeSendMessage)
	ai.RegisterExecutor("send_dm", executeSendDM)
	ai.RegisterExecutor("send_group_chat", executeSendGroupChat)
	ai.RegisterExecutor("set_reminder", executeSetReminder)
	ai.RegisterExecutor("summarize_channel", executeSummarizeChannel)
	ai.RegisterExecutor("summarize_dm", executeSummarizeDM)
	ai.RegisterExecutor("summarize_group_chat", executeSummarizeGroupChat)
	ai.RegisterExecutor("web_search", executeWebSearch)
	ai.RegisterExecutor("run_analysis", executeRunAnalysis)
	RegisterSearchExecutors()

	// Tables (first-class structured data): list / read / create row / update row.
	registerTableExecutors()

	// External, read-only data sources: list / read schema / query (pushdown).
	registerDataSourceExecutors()

	// Per-user connectors (Gmail, Google Calendar, GitHub).
	RegisterConnectorExecutors()

	// Read-only code understanding over the workspace's connected GitHub repo.
	RegisterCodeExecutors()
}

// --- Executor implementations ---

// executeSendMessage sends a message (post) to a channel.
func executeSendMessage(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	channelUUID := action.Params["channel_uuid"]
	text := action.Params["text"]

	if channelUUID == "" {
		return "", nil, fmt.Errorf("channel_uuid is required")
	}
	if text == "" {
		return "", nil, fmt.Errorf("text is required")
	}

	// Intercept hallucinated send_message calls for users
	if strings.HasPrefix(channelUUID, "@") || len(channelUUID) < 30 {
		helpers.MessageLogs.InfoLog.Printf("AI routed send_message to send_dm for %s", channelUUID)
		action.Params["to_uuid"] = channelUUID
		return executeSendDM(ctx, action, userUUID)
	}

	// Look up user info (postgres + dgraph)
	userInfo, err := getUserInfoForExecutor(ctx, userUUID)
	if err != nil {
		return "", nil, fmt.Errorf("failed to look up user: %w", err)
	}

	// Look up channel via the cached basic-info query (same one the CreatePost
	// controller uses) and enforce the identical gates: the channel must exist
	// and not be deleted, the user must be a member, and in an announcement
	// (admins_only) channel only channel admins may post.
	dgraphChannel, err := channelDomain.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)
	if err != nil || dgraphChannel == nil || dgraphChannel.Uuid == "" {
		return "", nil, fmt.Errorf("channel not found or you don't have access")
	}
	if dgraphChannel.DeletedAt != nil && !dgraphChannel.DeletedAt.IsZero() {
		return "", nil, fmt.Errorf("channel not found or you don't have access")
	}
	if dgraphChannel.IsMember == 0 {
		return "", nil, fmt.Errorf("you are not a member of this channel")
	}
	if dgraphChannel.PostPolicy == "admins_only" && dgraphChannel.IsAdmin == 0 {
		return "", nil, fmt.Errorf("only channel admins can post in this announcement channel")
	}

	// Render the model's text rather than wrapping it. A single <p> collapses
	// every newline to a space, so a message written as several lines or a list
	// arrived as one run-on sentence. modelTextToHTML strips tags and escapes
	// before adding any markup of its own.
	htmlText := MarkAIGenerated(modelTextToHTML(text))
	channelParsedUUID, err := uuid.Parse(channelUUID)
	if err != nil {
		return "", nil, fmt.Errorf("invalid channel UUID: %w", err)
	}

	postInfo := &postAdapter.InputCreateOrUpdatePostInfo{
		HTMLText:    htmlText,
		ChannelUuid: channelUUID,       // string — used by notification flow
		ChannelUUID: channelParsedUUID, // uuid.UUID — used by postgres
	}

	// Create the post
	createdPost, err := postBusiness.CreatePost(ctx, postInfo, userInfo, nil, dgraphChannel)
	if err != nil {
		return "", nil, fmt.Errorf("failed to send message: %w", err)
	}

	channelName := dgraphChannel.Name
	if channelName == "" {
		channelName = channelUUID
	}

	// Return metadata so the FE can dispatch createPostLocally
	actionData := map[string]string{
		"tool":            "send_message",
		"channel_uuid":    channelUUID,
		"post_uuid":       createdPost.Uuid,
		"post_text":       htmlText,
		"post_created_at": createdPost.PostCreated.Format(time.RFC3339),
	}

	return fmt.Sprintf("✅ Message sent to #%s", channelName), actionData, nil
}

// executeSendDM sends a direct message to another user.
func executeSendDM(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	toUUIDStr := action.Params["to_uuid"]
	text := action.Params["text"]

	if toUUIDStr == "" {
		return "", nil, fmt.Errorf("to_uuid is required")
	}
	if strings.TrimSpace(text) == "" {
		return "", nil, fmt.Errorf("text cannot be empty")
	}

	toUUID, err := uuid.Parse(toUUIDStr)
	if err != nil {
		// Fallback: The LLM hallucinated a username (e.g., "to_uuid": "cannabisd")
		// Let's try to find the user by their username using DGraph search
		cleanUname := strings.TrimPrefix(toUUIDStr, "@")
		foundUsers, uErr := domainUser.GetUserListWithSearchText(ctx, userUUID, cleanUname)
		if uErr == nil && len(foundUsers) > 0 {
			toUUIDStr = foundUsers[0].Uuid
			var parseErr error
			toUUID, parseErr = uuid.Parse(toUUIDStr)
			if parseErr != nil {
				return "", nil, fmt.Errorf("resolved user has invalid UUID '%s': %w", toUUIDStr, parseErr)
			}
		} else {
			return "", nil, fmt.Errorf("could not find user '%s' in workspace", toUUIDStr)
		}
	}

	// Look up sender info
	userInfo, err := getUserInfoForExecutor(ctx, userUUID)
	if err != nil {
		return "", nil, fmt.Errorf("failed to look up sender: %w", err)
	}

	// Look up recipient info, then apply the SHIPPED recipient rule.
	//
	// This used to check resolution and soft-deletion only, and omitted the external
	// test controllers/Chat.CreateChat has always enforced — so an agent could DM an
	// attribution-only ghost identity that a person using the app is refused. Both
	// paths now ask business/Principal the same question, so they cannot drift again.
	sendToDgraphInfo, err := userBusiness.GetDgraphUserInfoByUUID(ctx, toUUIDStr)
	if err != nil {
		return "", nil, fmt.Errorf("recipient not found")
	}
	if e := principalBusiness.CanReceiveDirectMessage(sendToDgraphInfo); !e.Allowed {
		return "", nil, fmt.Errorf("%s", e.Reason)
	}

	// See executeSendMessage: rendered, not wrapped, so newlines survive.
	htmlText := MarkAIGenerated(modelTextToHTML(text))
	mentions, err := helpers.GetMentions(htmlText)
	if err != nil {
		return "", nil, fmt.Errorf("failed to parse mentions: %w", err)
	}

	mentionsDgraphUsersList, err := userBusiness.GetDgraphUserInfoByUUIDs(ctx, mentions)
	if len(mentionsDgraphUsersList) != len(mentions) {
		return "", nil, fmt.Errorf("invalid users in mentions")
	}

	// Prepare chat info
	chatInput := &chatAdapter.ChatInfo{
		TextHtml: htmlText,
		ToUuid:   toUUIDStr,
	}

	// Create DM
	createdChatInfo, err := chatBusiness.CreateChat(ctx, chatInput, userInfo, sendToDgraphInfo, toUUID, mentionsDgraphUsersList)
	if err != nil {
		return "", nil, fmt.Errorf("failed to send DM: %w", err)
	}

	// Build action data for frontend Redux dispatch
	actionData := map[string]string{
		"tool":            "send_dm",
		"dm_grouping_id":  helpers.GetGroupingId(userInfo.UserPostgresInfo.Id.String(), toUUIDStr),
		"chat_uuid":       createdChatInfo.Uuid,
		"chat_text":       htmlText,
		"chat_created_at": createdChatInfo.ChatCreated.Format(time.RFC3339),
		"recipient_uuid":  toUUIDStr,
		"recipient_name":  sendToDgraphInfo.UserFullName,
	}

	return fmt.Sprintf("✅ DM sent to %s", sendToDgraphInfo.UserFullName), actionData, nil
}

// executeSendGroupChat sends a message in a group chat.
func executeSendGroupChat(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	grpID := action.Params["grp_id"]
	text := action.Params["text"]

	if grpID == "" {
		return "", nil, fmt.Errorf("grp_id is required")
	}
	if strings.TrimSpace(text) == "" {
		return "", nil, fmt.Errorf("text cannot be empty")
	}

	// Look up sender info
	userInfo, err := getUserInfoForExecutor(ctx, userUUID)
	if err != nil {
		return "", nil, fmt.Errorf("failed to look up sender: %w", err)
	}

	// Verify group membership and get participants
	dgraphDm, err := chatBusiness.GetDgraphDmBasicInfoFromDgraph(ctx, userInfo.UserDgraphInfo.Uid, grpID)
	if err != nil || dgraphDm == nil {
		return "", nil, fmt.Errorf("group not found")
	}
	if dgraphDm.ParticipantIsMember == 0 {
		return "", nil, fmt.Errorf("not authorized to send to this group")
	}

	// See executeSendMessage: rendered, not wrapped, so newlines survive.
	htmlText := MarkAIGenerated(modelTextToHTML(text))
	mentions, err := helpers.GetMentions(htmlText)
	if err != nil {
		return "", nil, fmt.Errorf("failed to parse mentions: %w", err)
	}

	mentionsDgraphUsersList, err := userBusiness.GetDgraphUserInfoByUUIDs(ctx, mentions)
	if len(mentionsDgraphUsersList) != len(mentions) {
		return "", nil, fmt.Errorf("invalid users in mentions")
	}

	// Prepare chat info
	chatInput := &chatAdapter.ChatInfo{
		TextHtml: htmlText,
		GrpUuid:  grpID,
	}

	// Create Group Chat message
	createdChatInfo, err := chatBusiness.CreateChatForGroup(ctx, chatInput, userInfo, mentionsDgraphUsersList, dgraphDm.Participants)
	if err != nil {
		return "", nil, fmt.Errorf("failed to send group chat: %w", err)
	}

	// Build action data for frontend Redux dispatch
	actionData := map[string]string{
		"tool":            "send_group_chat",
		"grp_id":          grpID,
		"chat_uuid":       createdChatInfo.Uuid,
		"chat_text":       htmlText,
		"chat_created_at": createdChatInfo.ChatCreated.Format(time.RFC3339),
	}

	participantCount := len(dgraphDm.Participants)
	return fmt.Sprintf("✅ Message sent to group chat (%d members)", participantCount), actionData, nil
}

// executeCreateTask creates a task in a project.
func executeCreateTask(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	taskName := action.Params["task_name"]
	projectUUID := action.Params["project_uuid"]
	description := action.Params["description"]
	priority := normalizeTaskPriority(action.Params["priority"])

	if taskName == "" {
		return "", nil, fmt.Errorf("task_name is required")
	}
	if projectUUID == "" {
		return "", nil, fmt.Errorf("project_uuid is required")
	}

	// Look up user info
	userInfo, err := getUserInfoForExecutor(ctx, userUUID)
	if err != nil {
		return "", nil, fmt.Errorf("failed to look up user: %w", err)
	}

	// Parse project UUID
	projectParsedUUID, err := uuid.Parse(projectUUID)
	if err != nil {
		return "", nil, fmt.Errorf("invalid project UUID: %w", err)
	}

	// Look up project DGraph info. Use the lightweight basic-info query (the
	// same one controllers/Task/CreateTask uses) - it carries the admin flag,
	// team uid, name, and uuid that CreateTask needs, without pulling every
	// task + comment in the project the way the full project query would.
	dgraphProject, err := projectDomain.GetBasicDgraphProjectInfo(ctx, projectUUID, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		return "", nil, fmt.Errorf("failed to look up project: %w", err)
	}
	if dgraphProject == nil || dgraphProject.Uuid == "" {
		return "", nil, fmt.Errorf("project not found or you don't have access")
	}
	if dgraphProject.IsProjectAdmin == 0 {
		return "", nil, fmt.Errorf("you must be an admin of project '%s' to create tasks", dgraphProject.Name)
	}

	// Optional assignee. The model may pass a uuid or (when it couldn't recall
	// one) a name; resolve either, mirroring assign_task / send_dm. A nil
	// result means "leave unassigned".
	assigneeDgraph, err := resolveAssigneeRef(ctx, userUUID, action.Params["assignee_uuid"])
	if err != nil {
		return "", nil, err
	}

	// Build task input
	taskInput := taskAdapter.CreateOrUpdateTaskInput{
		TaskName:        taskName,
		TaskDescription: description,
		Priority:        priority,
		Status:          "todo",
	}

	// Create the task
	_, err = taskBusiness.CreateTask(ctx, projectParsedUUID, userInfo, dgraphProject, assigneeDgraph, taskInput, nil)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create task: %w", err)
	}

	projectName := dgraphProject.Name
	if projectName == "" {
		projectName = projectUUID
	}

	msg := fmt.Sprintf("✅ Task \"%s\" created in project %s (priority: %s)", taskName, projectName, priority)
	if assigneeDgraph != nil {
		msg = fmt.Sprintf("✅ Task \"%s\" created in project %s (priority: %s), assigned to %s", taskName, projectName, priority, assigneeDgraph.UserName)
	}
	return msg, nil, nil
}

// normalizeTaskPriority maps whatever the model emits for a task priority onto
// OneCamp's three valid values (low/medium/high), absorbing common synonyms
// (urgent, p1, ...) so a hallucinated priority can never reach the task layer.
// Unknown/empty values default to medium.
func normalizeTaskPriority(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "high", "urgent", "critical", "highest", "very high", "p0", "p1":
		return dgraphStruct.TASK_PRIORITY_HIGH
	case "low", "lowest", "very low", "minor", "p4", "p5":
		return dgraphStruct.TASK_PRIORITY_LOW
	default:
		return dgraphStruct.TASK_PRIORITY_MEDIUM
	}
}

// resolveAssigneeRef turns an assignee reference the model supplied - a user
// UUID, or (when it could not recall the id) an @name / name - into a DGraph
// user. An empty ref means "no assignee" (nil, nil). This mirrors executeSendDM's
// self-healing so "assign it to John" works even when the model passes a name
// instead of a UUID, and is shared by create_task and assign_task.
func resolveAssigneeRef(ctx context.Context, actingUserUUID, ref string) (*dgraphStruct.DgraphUser, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, nil
	}
	if _, err := uuid.Parse(ref); err == nil {
		u, uerr := userBusiness.GetDgraphUserInfoByUUID(ctx, ref)
		if uerr != nil || u == nil {
			return nil, fmt.Errorf("could not find the user to assign")
		}
		return u, nil
	}
	// Not a UUID: resolve by name, scoped to the acting user's workspace.
	cleanName := strings.TrimPrefix(ref, "@")
	found, err := domainUser.GetUserListWithSearchText(ctx, actingUserUUID, cleanName)
	if err != nil || len(found) == 0 {
		return nil, fmt.Errorf("could not find a user matching %q to assign", ref)
	}
	if len(found) == 1 {
		return found[0], nil
	}
	// Ambiguous: accept only a single exact (case-insensitive) name match.
	var exact *dgraphStruct.DgraphUser
	for _, u := range found {
		if u != nil && strings.EqualFold(strings.TrimSpace(u.UserName), cleanName) {
			if exact != nil {
				return nil, fmt.Errorf("more than one user matches %q - please be more specific", ref)
			}
			exact = u
		}
	}
	if exact != nil {
		return exact, nil
	}
	return nil, fmt.Errorf("more than one user matches %q - please be more specific", ref)
}

// loadTaskForUpdateAsAdmin loads a task as the acting user and enforces the
// same project-admin check the task-update HTTP endpoints use, so an agent
// action can never do more than the user could do by hand.
func loadTaskForUpdateAsAdmin(ctx context.Context, taskUUIDStr, userUUID string) (*userModels.UserInfo, *dgraphStruct.DgraphTask, error) {
	if strings.TrimSpace(taskUUIDStr) == "" {
		return nil, nil, fmt.Errorf("task_uuid is required")
	}
	if _, err := uuid.Parse(taskUUIDStr); err != nil {
		return nil, nil, fmt.Errorf("invalid task UUID")
	}
	userInfo, err := getUserInfoForExecutor(ctx, userUUID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to look up user: %w", err)
	}
	dgraphTask, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, userInfo.UserDgraphInfo.Uid)
	if err != nil || dgraphTask == nil {
		return nil, nil, fmt.Errorf("task not found or you don't have access")
	}
	if dgraphTask.Project == nil || dgraphTask.Project.IsProjectAdmin == 0 {
		return nil, nil, fmt.Errorf("you must be an admin of this task's project to change it")
	}
	return userInfo, dgraphTask, nil
}

// executeUpdateTaskStatus moves a task to a new status: a built-in one (todo,
// inProgress, inReview, done, backlog, canceled) or one of its project's own,
// by name.
func executeUpdateTaskStatus(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	taskUUIDStr := action.Params["task_uuid"]
	status := strings.TrimSpace(action.Params["status"])
	if status == "" {
		return "", nil, fmt.Errorf("status is required")
	}
	userInfo, dgraphTask, err := loadTaskForUpdateAsAdmin(ctx, taskUUIDStr, userUUID)
	if err != nil {
		return "", nil, err
	}
	taskUUID, _ := uuid.Parse(taskUUIDStr)

	if err := taskBusiness.UpdateTaskStatusByTaskUUID(ctx, taskUUID, status, dgraphTask, &userInfo.UserDgraphInfo); err != nil {
		if errors.Is(err, taskStatusBusiness.ErrUnknownStatus) {
			projectID := ""
			if dgraphTask.Project != nil {
				projectID = dgraphTask.Project.Uuid
			}
			return "", nil, fmt.Errorf("invalid status %q (use one of: %s)", status, taskStatusBusiness.Describe(ctx, projectID))
		}
		return "", nil, fmt.Errorf("failed to update task status: %w", err)
	}
	name := dgraphTask.Name
	if name == "" {
		name = taskUUIDStr
	}
	// Action data lets the FE patch the task in the visible list immediately,
	// so the status flips without a page refresh.
	actionData := map[string]string{
		"tool":      "update_task_status",
		"task_uuid": taskUUIDStr,
		"status":    status,
	}
	return fmt.Sprintf("✅ Moved task \"%s\" to %s", name, status), actionData, nil
}

// executeAssignTask sets (or, with an empty assignee, clears) a task's assignee.
func executeAssignTask(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	taskUUIDStr := action.Params["task_uuid"]

	userInfo, dgraphTask, err := loadTaskForUpdateAsAdmin(ctx, taskUUIDStr, userUUID)
	if err != nil {
		return "", nil, err
	}
	taskUUID, _ := uuid.Parse(taskUUIDStr)

	// Resolve the assignee from a uuid or a name (empty clears the assignee).
	newAssignee, err := resolveAssigneeRef(ctx, userUUID, action.Params["assignee_uuid"])
	if err != nil {
		return "", nil, err
	}
	assigneeName := "no one"
	if newAssignee != nil {
		assigneeName = newAssignee.UserName
	}

	oldAssigneeUID := ""
	if dgraphTask.Assignee != nil {
		oldAssigneeUID = dgraphTask.Assignee.Uid
	}

	if err := taskBusiness.UpdateTaskAssigneeByTaskUUID(ctx, taskUUID, newAssignee, oldAssigneeUID, dgraphTask.Uid, dgraphTask, &userInfo.UserDgraphInfo); err != nil {
		return "", nil, fmt.Errorf("failed to update task assignee: %w", err)
	}
	name := dgraphTask.Name
	if name == "" {
		name = taskUUIDStr
	}
	actionData := map[string]string{
		"tool":      "assign_task",
		"task_uuid": taskUUIDStr,
	}
	if newAssignee != nil {
		actionData["assignee_uuid"] = newAssignee.Uuid
		actionData["assignee_name"] = newAssignee.UserName
		actionData["assignee_full_name"] = newAssignee.UserFullName
	}
	return fmt.Sprintf("✅ Assigned task \"%s\" to %s", name, assigneeName), actionData, nil
}

// executeSetTaskDueDate sets a task's due date (RFC3339; empty clears it).
func executeSetTaskDueDate(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	taskUUIDStr := action.Params["task_uuid"]
	dueStr := strings.TrimSpace(action.Params["due_date"])

	due := time.Time{}
	if dueStr != "" {
		t, perr := time.Parse(time.RFC3339, dueStr)
		if perr != nil {
			return "", nil, fmt.Errorf("due_date must be RFC3339 (e.g. 2026-06-20T17:00:00Z)")
		}
		due = t
	}

	userInfo, dgraphTask, err := loadTaskForUpdateAsAdmin(ctx, taskUUIDStr, userUUID)
	if err != nil {
		return "", nil, err
	}
	taskUUID, _ := uuid.Parse(taskUUIDStr)

	if err := taskBusiness.UpdateTaskDueDateByTaskUUID(ctx, taskUUID, &due, dgraphTask, &userInfo.UserDgraphInfo); err != nil {
		return "", nil, fmt.Errorf("failed to update due date: %w", err)
	}
	name := dgraphTask.Name
	if name == "" {
		name = taskUUIDStr
	}
	actionData := map[string]string{
		"tool":      "set_task_due_date",
		"task_uuid": taskUUIDStr,
		"due_date":  dueStr,
	}
	if dueStr == "" {
		return fmt.Sprintf("✅ Cleared the due date on task \"%s\"", name), actionData, nil
	}
	return fmt.Sprintf("✅ Set task \"%s\" due %s", name, due.Format("Jan 2, 2006 15:04 MST")), actionData, nil
}

// executeCreateDoc creates a new document using the Doc business layer. When a
// body is supplied, it is converted from Markdown to the Tiptap-safe HTML
// subset and persisted to doc_body. A brand-new doc is open in no editor, so
// the collaboration service loads exactly this body the first time someone
// opens the doc - which is what makes "draft a PRD from this thread" produce a
// populated doc instead of an empty titled shell.
func executeCreateDoc(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	title := helpers.HTMLToPlainText(action.Params["title"])
	isPrivateStr := action.Params["is_private"]
	body := action.Params["body"]

	if title == "" {
		return "", nil, fmt.Errorf("title is required")
	}

	isPrivate := false
	switch strings.ToLower(strings.TrimSpace(isPrivateStr)) {
	case "true", "yes", "1", "private":
		isPrivate = true
	}

	// Look up user's DGraph info
	dgraphUser, err := getDgraphUserForExecutor(ctx, userUUID)
	if err != nil {
		return "", nil, err
	}

	inputDoc := &docAdapter.InputCreateDoc{
		DocTitle:   title,
		DocPrivate: isPrivate,
	}

	doc, err := docBusiness.CreateDoc(ctx, dgraphUser, inputDoc)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create doc: %w", err)
	}
	if doc == nil {
		return "", nil, fmt.Errorf("doc creation returned nil result")
	}

	// Persist the body, if any. Failure here is non-fatal: the (titled) doc
	// already exists, so we surface a softer message rather than erroring out.
	if strings.TrimSpace(body) != "" {
		html := docMarkdownToHTML(body)
		if html != "" {
			updateErr := docBusiness.UpdateDoc(ctx, &docAdapter.InputUpdateDoc{
				DocId: doc.Uuid,
				Body:  &html,
			})
			if updateErr != nil {
				helpers.LogErrorWithContext(ctx, "executeCreateDoc: created doc %s but failed to set body: %v", doc.Uuid, updateErr)
				return fmt.Sprintf("✅ Document \"%s\" created. I could not write the body just now - you can add it in the doc.", title), nil, nil
			}
		}
	}

	return fmt.Sprintf("✅ Document \"%s\" created successfully", title), nil, nil
}

// executeSetReminder creates a calendar event as a reminder.
func executeSetReminder(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	title := action.Params["title"]
	startTimeStr := action.Params["start_time"]

	// Backward compatibility: accept old params from cached LLM
	if title == "" {
		title = action.Params["task_name"]
	}
	if startTimeStr == "" {
		startTimeStr = action.Params["due_date"]
	}

	if title == "" {
		return "", nil, fmt.Errorf("title is required")
	}
	if startTimeStr == "" {
		return "", nil, fmt.Errorf("start_time is required (RFC3339 format)")
	}

	startTime, err := time.Parse(time.RFC3339, startTimeStr)
	if err != nil {
		return "", nil, fmt.Errorf("invalid start_time format, expected RFC3339 (e.g. 2026-03-25T18:00:00Z): %w", err)
	}

	// Default: 1-hour event
	endTime := startTime.Add(1 * time.Hour)

	// Look up full user info (postgres + dgraph needed for CreateEvent)
	userInfo, err := getUserInfoForExecutor(ctx, userUUID)
	if err != nil {
		return "", nil, fmt.Errorf("failed to look up user: %w", err)
	}

	description := action.Params["description"]
	if description == "" {
		description = "Created by AI Assistant"
	}

	eventInput := calendarAdapter.CreateOrUpdateEventInput{
		Title:                title,
		Description:          description,
		StartTime:            startTime.Format(time.RFC3339),
		EndTime:              endTime.Format(time.RFC3339),
		SyncToGoogleCalendar: true,
	}

	createdEvent, err := calendarBusiness.CreateEvent(ctx, userInfo, eventInput)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create calendar event: %w", err)
	}

	actionData := map[string]string{
		"tool":       "set_reminder",
		"event_uuid": createdEvent.EventUuid,
		"title":      title,
		"start_time": startTime.Format(time.RFC3339),
		"end_time":   endTime.Format(time.RFC3339),
	}

	return fmt.Sprintf("✅ Reminder \"%s\" created for %s", title, startTime.Format("Jan 2, 2006 3:04 PM")), actionData, nil
}

// executeSummarizeChannel fetches and summarizes recent messages in a channel.
func executeSummarizeChannel(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	channelUUID := action.Params["channel_uuid"]
	countStr := action.Params["count"]

	if channelUUID == "" {
		return "", nil, fmt.Errorf("channel_uuid is required")
	}
	if _, err := uuid.Parse(strings.TrimSpace(channelUUID)); err != nil {
		return "", nil, fmt.Errorf("invalid channel id")
	}

	count := 0
	if countStr != "" {
		fmt.Sscanf(countStr, "%d", &count)
	}

	// Look up user info (required for SummarizeChannel)
	userInfo, err := getUserInfoForExecutor(ctx, userUUID)
	if err != nil {
		return "", nil, fmt.Errorf("failed to look up user: %w", err)
	}

	// Call the generic summarization logic with localization context from ctx
	loc := ai.GetLocalization(ctx)
	resp, err := SummarizeChannel(ctx, userInfo, channelUUID, count, loc)
	if err != nil {
		return "", nil, fmt.Errorf("summarization failed: %w", err)
	}

	return resp.Summary, nil, nil
}

// executeSummarizeDM fetches and summarizes recent messages in a DM.
func executeSummarizeDM(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	toUserUUID := action.Params["to_user_uuid"]
	countStr := action.Params["count"]

	if toUserUUID == "" {
		return "", nil, fmt.Errorf("to_user_uuid is required")
	}
	if _, err := uuid.Parse(strings.TrimSpace(toUserUUID)); err != nil {
		return "", nil, fmt.Errorf("invalid user id")
	}

	count := 0
	if countStr != "" {
		fmt.Sscanf(countStr, "%d", &count)
	}

	userInfo, err := getUserInfoForExecutor(ctx, userUUID)
	if err != nil {
		return "", nil, fmt.Errorf("failed to look up user: %w", err)
	}

	loc := ai.GetLocalization(ctx)
	resp, err := SummarizeDM(ctx, userInfo, toUserUUID, count, loc)
	if err != nil {
		return "", nil, fmt.Errorf("DM summarization failed: %w", err)
	}

	return resp.Summary, nil, nil
}

// executeSummarizeGroupChat fetches and summarizes recent messages in a group chat.
//
// The conversation is read from grp_id, the parameter the tool declares, and
// from nothing else: a call is authorised by what its declared parameters
// name (business/MCPServer), and an undeclared alias read first let a call
// pass that check naming one conversation and summarize another.
func executeSummarizeGroupChat(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	groupingId := strings.TrimSpace(action.Params["grp_id"])
	countStr := action.Params["count"]

	if groupingId == "" {
		return "", nil, fmt.Errorf("grp_id is required")
	}

	count := 0
	if countStr != "" {
		fmt.Sscanf(countStr, "%d", &count)
	}

	userInfo, err := getUserInfoForExecutor(ctx, userUUID)
	if err != nil {
		return "", nil, fmt.Errorf("failed to look up user: %w", err)
	}

	loc := ai.GetLocalization(ctx)
	resp, err := SummarizeGroupChat(ctx, userInfo, groupingId, count, loc)
	if err != nil {
		return "", nil, fmt.Errorf("group chat summarization failed: %w", err)
	}

	return resp.Summary, nil, nil
}

// --- Helper functions ---

// getUserInfoForExecutor builds the full UserInfo (postgres + dgraph) from a UUID.
// This is needed because business functions like CreatePost and CreateTask
// require the full UserInfo struct with both postgres and dgraph data.
func getUserInfoForExecutor(ctx context.Context, userUUID string) (*userModels.UserInfo, error) {
	parsedUUID, err := uuid.Parse(userUUID)
	if err != nil {
		return nil, fmt.Errorf("invalid user UUID: %w", err)
	}

	// Get postgres user info
	postgresUser, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, parsedUUID)
	if err != nil {
		return nil, fmt.Errorf("failed to get postgres user: %w", err)
	}
	if postgresUser == nil {
		return nil, fmt.Errorf("user not found in postgres: %s", userUUID)
	}

	// Get dgraph user info
	dgraphUser, err := getDgraphUserForExecutor(ctx, userUUID)
	if err != nil {
		return nil, err
	}

	return &userModels.UserInfo{
		UserPostgresInfo: *postgresUser,
		UserDgraphInfo:   *dgraphUser,
	}, nil
}

// getDgraphUserForExecutor looks up a user's basic DGraph info by UUID.
func getDgraphUserForExecutor(ctx context.Context, userUUID string) (*dgraphStruct.DgraphUser, error) {
	user, err := userDomain.GetDgraphUserInfoByUUID(ctx, userUUID)
	if err != nil {
		return nil, fmt.Errorf("failed to look up user: %w", err)
	}
	if user == nil {
		return nil, fmt.Errorf("user not found: %s", userUUID)
	}
	return user, nil
}

// BuildUserInfoByUserUUID builds a full UserInfo (postgres + dgraph) for a
// user UUID. Exported for callers outside this package (e.g. the AI coworker
// mention responder) that must run AskAI AS a specific user so the answer
// respects that user's workspace visibility — never the bot's.
func BuildUserInfoByUserUUID(ctx context.Context, userUUID string) (*userModels.UserInfo, error) {
	return getUserInfoForExecutor(ctx, userUUID)
}

// init registers executors automatically when the package is imported.
func init() {
	RegisterToolExecutors()
}
