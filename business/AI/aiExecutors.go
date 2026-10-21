package business

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	docAdapter "github.com/akashc777/OneCamp/adapter/Doc"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	taskAdapter "github.com/akashc777/OneCamp/adapter/Task"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	principalBusiness "github.com/akashc777/OneCamp/business/Principal"
	savedItemBusiness "github.com/akashc777/OneCamp/business/SavedItem"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	taskPriority "github.com/akashc777/OneCamp/business/TaskPriority"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
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
	ai.RegisterExecutor("doc_insert_after", executeDocInsertAfter)
	ai.RegisterExecutor("doc_replace_section", executeDocReplaceSection)
	ai.RegisterExecutor("doc_suggest", executeDocSuggest)
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
	ai.RegisterExecutor("list_docs", executeListDocs)

	// The work graph: what a thing is connected to, and how two connect.
	ai.RegisterExecutor("related_items", executeRelatedItems)
	ai.RegisterExecutor("how_connected", executeHowConnected)

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
	// Through an API token (MCP, or an agent run a token asked for), never as one
	// of its admins: they post in it in the app, as themselves.
	if dgraphChannel.PostPolicy == "admins_only" && helpers.ActsThroughToken(ctx) {
		return "", nil, fmt.Errorf("only channel admins can post in this announcement channel, in the app: a token posts as a member")
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
		// The model gave a name or an @handle (e.g. "to_uuid": "cannabisd")
		// rather than an id. The search finds people by display name, full
		// name or handle, so a name can match several: it is used only when
		// it plainly means one of them (personNamed, as for an assignee),
		// and otherwise the model is told who matched, to ask or to choose.
		cleanUname := strings.TrimPrefix(strings.TrimSpace(toUUIDStr), "@")
		foundUsers, uErr := domainUser.GetUserListWithSearchText(ctx, userUUID, cleanUname)
		foundUsers = demoReachable(ctx, userUUID, foundUsers)
		if uErr != nil || len(foundUsers) == 0 {
			return "", nil, fmt.Errorf("could not find user '%s' in workspace", toUUIDStr)
		}
		to := personNamed(foundUsers, cleanUname, false)
		if to == nil {
			return "", nil, fmt.Errorf("more than one person matches %q: %s. Ask which one is meant, or send to their @handle",
				toUUIDStr, peopleByNameAndHandle(foundUsers))
		}
		toUUIDStr = to.Uuid
		var parseErr error
		toUUID, parseErr = uuid.Parse(toUUIDStr)
		if parseErr != nil {
			return "", nil, fmt.Errorf("resolved user has invalid UUID '%s': %w", toUUIDStr, parseErr)
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
	if err := demoRefuses(ctx, userUUID, sendToDgraphInfo); err != nil {
		return "", nil, err
	}

	// See executeSendMessage: rendered, not wrapped, so newlines survive.
	htmlText := MarkAIGenerated(modelTextToHTML(text))
	mentions, err := helpers.GetMentions(htmlText)
	if err != nil {
		return "", nil, fmt.Errorf("failed to parse mentions: %w", err)
	}

	mentionsDgraphUsersList, err := userBusiness.GetDgraphUserInfoByUUIDs(ctx, mentions)
	if err != nil {
		return "", nil, fmt.Errorf("failed to read the people mentioned: %w", err)
	}
	if len(mentionsDgraphUsersList) != len(mentions) {
		return "", nil, fmt.Errorf("invalid users in mentions")
	}
	if err := demoRefuses(ctx, userUUID, mentionsDgraphUsersList...); err != nil {
		return "", nil, err
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
	if err != nil {
		return "", nil, fmt.Errorf("failed to read the people mentioned: %w", err)
	}
	if len(mentionsDgraphUsersList) != len(mentions) {
		return "", nil, fmt.Errorf("invalid users in mentions")
	}
	if err := demoRefuses(ctx, userUUID, mentionsDgraphUsersList...); err != nil {
		return "", nil, err
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
	if !dgraphProject.ReaderIsIn() {
		return "", nil, fmt.Errorf("project not found or you don't have access")
	}
	standing, err := workAccess.Of(ctx, userInfo.UserDgraphInfo.Uuid, dgraphProject)
	if err != nil {
		return "", nil, fmt.Errorf("couldn't check what you can do in project '%s': %w", dgraphProject.Name, err)
	}
	if err := standing.Check(workAccess.NeedCreateTask); err != nil {
		return "", nil, fmt.Errorf("project '%s': %w", dgraphProject.Name, err)
	}

	// Optional assignee. The model may pass a uuid or (when it couldn't recall
	// one) a name; resolve either, mirroring assign_task / send_dm. A nil
	// result means "leave unassigned".
	assigneeDgraph, err := resolveAssigneeRef(ctx, userUUID, action.Params["assignee_uuid"], false)
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
// a priority a task can have (dgraphStruct.TaskPriorityFromWords), so a
// hallucinated priority can never reach the task layer. Unknown and empty
// values are medium.
func normalizeTaskPriority(p string) string {
	if got, ok := taskPriority.FromWords(p); ok && got != "" {
		return got
	}
	return dgraphStruct.TASK_PRIORITY_MEDIUM
}

// resolveAssigneeRef turns an assignee reference the model supplied - a user
// UUID, or (when it could not recall the id) an @name / name - into a DGraph
// user. An empty ref means "no assignee" (nil, nil). A name is resolved as a
// direct message's recipient is (personNamed), so "assign it to John" works
// even when the model passes a name instead of a UUID. Shared by create_task
// and assign_task. strict is for an invitation, which shows someone the event:
// only a name that is exactly theirs counts (personNamed).
func resolveAssigneeRef(ctx context.Context, actingUserUUID, ref string, strict bool) (*dgraphStruct.DgraphUser, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, nil
	}
	if _, err := uuid.Parse(ref); err == nil {
		u, uerr := userBusiness.GetDgraphUserInfoByUUID(ctx, ref)
		if uerr != nil || u == nil {
			return nil, fmt.Errorf("could not find the user to assign")
		}
		if err := demoRefuses(ctx, actingUserUUID, u); err != nil {
			return nil, err
		}
		return u, nil
	}
	// Not a UUID: resolve by name, scoped to the acting user's workspace.
	cleanName := strings.TrimPrefix(ref, "@")
	found, err := domainUser.GetUserListWithSearchText(ctx, actingUserUUID, cleanName)
	found = demoReachable(ctx, actingUserUUID, found)
	if err != nil || len(found) == 0 {
		return nil, fmt.Errorf("could not find a user matching %q to assign", ref)
	}
	if u := personNamed(found, strings.TrimSpace(cleanName), strict); u != nil {
		return u, nil
	}
	return nil, fmt.Errorf("more than one user matches %q - please be more specific", ref)
}

// personNamed is the one of found, the people a search for name turned up,
// whom name plainly means, ignoring case: the only one; else the one whose
// handle it is; else the only one whose display name it is. strict (an
// invitation: "Sam" never invites Samantha) never takes a lone hit, and
// after the handle takes a whole name, display or full, then a first name.
// nil when it could be more than one of them. Pure.
func personNamed(found []*dgraphStruct.DgraphUser, name string, strict bool) *dgraphStruct.DgraphUser {
	if !strict && len(found) == 1 {
		return found[0]
	}
	handle := func(u *dgraphStruct.DgraphUser) bool { return u.Handle != "" && strings.EqualFold(u.Handle, name) }
	displayName := func(u *dgraphStruct.DgraphUser) bool { return strings.EqualFold(strings.TrimSpace(u.UserName), name) }
	rules := []func(u *dgraphStruct.DgraphUser) bool{handle, displayName}
	if strict {
		rules = []func(u *dgraphStruct.DgraphUser) bool{
			handle,
			func(u *dgraphStruct.DgraphUser) bool {
				return displayName(u) || strings.EqualFold(strings.TrimSpace(u.UserFullName), name)
			},
			func(u *dgraphStruct.DgraphUser) bool {
				for _, n := range []string{u.UserName, u.UserFullName} {
					if f := strings.Fields(n); len(f) > 0 && strings.EqualFold(f[0], name) {
						return true
					}
				}
				return false
			},
		}
	}
	for _, rule := range rules {
		var match *dgraphStruct.DgraphUser
		for _, u := range found {
			if u == nil || !rule(u) {
				continue
			}
			if match != nil && match.Uuid != u.Uuid {
				return nil // two people answer to it
			}
			match = u
		}
		if match != nil {
			return match
		}
	}
	return nil
}

// ambiguousPeopleShown bounds the people an ambiguous name lists.
const ambiguousPeopleShown = 10

// peopleByNameAndHandle lists people for the model to choose from, each by
// their name and @handle: the one name rule without its last step, so never
// part of an address, which the model would repeat. Someone with neither is
// left out, since they can be named only by their address. Pure.
func peopleByNameAndHandle(people []*dgraphStruct.DgraphUser) string {
	var out []string
	for _, u := range people {
		if u == nil {
			continue
		}
		name := helpers.PersonDisplayName(u.UserName, u.UserFullName, "")
		switch {
		case name != "" && u.Handle != "":
			name += " (@" + u.Handle + ")"
		case u.Handle != "":
			name = "@" + u.Handle
		case name == "":
			continue
		}
		out = append(out, name)
	}
	if len(out) > ambiguousPeopleShown {
		out = append(out[:ambiguousPeopleShown], fmt.Sprintf("and %d more", len(out)-ambiguousPeopleShown))
	}
	return strings.Join(out, ", ")
}

// loadTaskForUpdate loads a task as the acting user and enforces the check
// the task-update HTTP endpoints use (Work Access), so an agent action can
// never do more than the user could do by hand.
func loadTaskForUpdate(ctx context.Context, taskUUIDStr, userUUID string) (*userModels.UserInfo, *dgraphStruct.DgraphTask, error) {
	if strings.TrimSpace(taskUUIDStr) == "" {
		return nil, nil, fmt.Errorf("task_uuid is required")
	}
	// The task's id, or its key ("Q4-12").
	taskID, err := taskKeys.Ref(ctx, taskUUIDStr)
	if errors.Is(err, taskKeys.ErrNoTask) {
		return nil, nil, fmt.Errorf("task not found or you don't have access")
	}
	if err != nil {
		return nil, nil, fmt.Errorf("couldn't look the task up: %w", err)
	}
	userInfo, err := getUserInfoForExecutor(ctx, userUUID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to look up user: %w", err)
	}
	dgraphTask, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskID.String(), userInfo.UserDgraphInfo.Uid)
	if err != nil || dgraphTask == nil {
		return nil, nil, fmt.Errorf("task not found or you don't have access")
	}
	standing, facts, err := workAccess.OfTask(ctx, userInfo.UserDgraphInfo.Uuid, userInfo.UserDgraphInfo.Uid, dgraphTask)
	if err != nil {
		return nil, nil, fmt.Errorf("couldn't check what you can do with this task: %w", err)
	}
	if err := standing.CheckTask(workAccess.NeedEditTask, facts); err != nil {
		return nil, nil, err
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
	userInfo, dgraphTask, err := loadTaskForUpdate(ctx, taskUUIDStr, userUUID)
	if err != nil {
		return "", nil, err
	}
	taskUUID, _ := uuid.Parse(dgraphTask.Uuid)
	taskUUIDStr = dgraphTask.Uuid

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

	userInfo, dgraphTask, err := loadTaskForUpdate(ctx, taskUUIDStr, userUUID)
	if err != nil {
		return "", nil, err
	}
	taskUUID, _ := uuid.Parse(dgraphTask.Uuid)
	taskUUIDStr = dgraphTask.Uuid

	// Resolve the assignee from a uuid or a name (empty clears the assignee).
	newAssignee, err := resolveAssigneeRef(ctx, userUUID, action.Params["assignee_uuid"], false)
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

	userInfo, dgraphTask, err := loadTaskForUpdate(ctx, taskUUIDStr, userUUID)
	if err != nil {
		return "", nil, err
	}
	taskUUID, _ := uuid.Parse(dgraphTask.Uuid)
	taskUUIDStr = dgraphTask.Uuid

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
	dgraphUser, err := createDocUser(ctx, userUUID)
	if err != nil {
		return "", nil, err
	}

	inputDoc := &docAdapter.InputCreateDoc{
		DocTitle:   title,
		DocPrivate: isPrivate,
	}

	doc, err := createDocAs(ctx, dgraphUser, inputDoc)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create doc: %w", err)
	}
	if doc == nil {
		return "", nil, fmt.Errorf("doc creation returned nil result")
	}
	// The new doc's id goes back to the model, so it can add to the doc it made
	// (doc_insert_after, append_to_doc) without searching for it.
	meta := map[string]string{"doc_uuid": doc.Uuid}
	made := fmt.Sprintf("Created the doc \"%s\" (doc_uuid %s).", title, doc.Uuid)

	// The body, if any, is the doc's first body, seeded into its live copy
	// (business/Doc SeedBodyLive) and credited to the person and the agent
	// acting for them. Failure here is not fatal: the titled doc exists.
	if strings.TrimSpace(body) != "" {
		if html := DocHTMLFromMarkdown(body); html != "" {
			if _, err := seedDocBody(ctx, doc.Uuid, html, docEditByOf(ctx, userUUID)); err != nil {
				helpers.LogErrorWithContext(ctx, "executeCreateDoc: created doc %s but failed to set body: %v", doc.Uuid, err)
				return made + " I couldn't write its body just now; add it in the doc.", meta, nil
			}
		}
	}
	return made, meta, nil
}

// saveLater keeps a Later item (business/SavedItem.Save); a seam for tests.
var saveLater = savedItemBusiness.Save

// createDocUser is the person a doc is made for: a seam for tests.
var createDocUser = getDgraphUserForExecutor

// createDocAs makes a doc owned by a person: a seam for tests.
var createDocAs = docBusiness.CreateDoc

// seedDocBody writes a new doc's first body into its live copy, never under a
// copy someone may already have open: a seam for tests.
var seedDocBody = docBusiness.SeedBodyLive

// executeSetReminder sets a reminder: a Later item due at the time, which
// comes back by itself then (it moves to the top of Later, an open app shows
// it, and a push reaches a closed one). It made a one-hour calendar event,
// which reminded nobody and showed the hour as busy to everyone booking it.
func executeSetReminder(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	title := strings.TrimSpace(action.Params["title"])
	startTimeStr := strings.TrimSpace(action.Params["start_time"])

	// Backward compatibility: accept old params from cached LLM
	if title == "" {
		title = strings.TrimSpace(action.Params["task_name"])
	}
	if startTimeStr == "" {
		startTimeStr = strings.TrimSpace(action.Params["due_date"])
	}

	if title == "" {
		return "", nil, fmt.Errorf("title is required")
	}
	if startTimeStr == "" {
		return "", nil, fmt.Errorf("start_time is required (RFC3339 format)")
	}

	at, err := time.Parse(time.RFC3339, startTimeStr)
	if err != nil {
		return "", nil, fmt.Errorf("invalid start_time format, expected RFC3339 (e.g. 2026-03-25T18:00:00Z): %w", err)
	}
	userID, err := uuid.Parse(userUUID)
	if err != nil {
		return "", nil, fmt.Errorf("invalid user")
	}

	item, err := saveLater(ctx, userID, savedItemBusiness.SaveInput{
		ItemType: "reminder",
		ItemID:   uuid.NewString(),
		Link:     "/app/later",
		Title:    title,
		Context:  strings.TrimSpace(action.Params["description"]),
		RemindAt: &at,
	})
	switch {
	case errors.Is(err, savedItemBusiness.ErrInvalid):
		return "That time has passed or is more than a year away, so there's nothing to remind you of then. Pick another time.", nil, nil
	case errors.Is(err, savedItemBusiness.ErrTooMany):
		return "Your Later list is full. Finish or remove a few items there, then ask again.", nil, nil
	case err != nil:
		return "", nil, fmt.Errorf("failed to set the reminder: %w", err)
	}

	when := reminderWhen(ctx, at)
	actionData := map[string]string{
		"tool":          "set_reminder",
		"saved_item_id": item.ID.String(),
		"title":         title,
		"remind_at":     at.UTC().Format(time.RFC3339),
	}
	return fmt.Sprintf("✅ I'll remind you: \"%s\", %s. It's in Later until then.", title, when), actionData, nil
}

// reminderWhen is a reminder's time as its person reads it: on their clock
// when the request says their zone ("13 Oct, 15:00"), else as the time was
// given, with its offset. It printed the model's offset, often UTC, in a US
// date format.
func reminderWhen(ctx context.Context, at time.Time) string {
	if loc, ok := helpers.FirstLocation(ai.GetLocalization(ctx)["timezone"]); ok {
		return at.In(loc).Format("2 Jan, 15:04")
	}
	if at.Location() == time.UTC {
		return at.Format("2 Jan, 15:04") + " UTC"
	}
	return at.Format("2 Jan, 15:04 (UTC-07:00)")
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

	u, err := userDomain.ActorByUUID(ctx, parsedUUID)
	if err != nil {
		return nil, fmt.Errorf("user not found: %s: %w", userUUID, err)
	}
	return u, nil
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
