package dgraphInit

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/dgraph-io/dgo/v230"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding/gzip"
)

type DgraphConfig struct {
	Endpoint string
	User     string
	Password string
}

var DgraphClient *dgo.Dgraph

func ConnectDgraph(ctx context.Context, dgraph *DgraphConfig) (err error, conn *grpc.ClientConn) {
	//ctx := context.Background()
	// Board canvas state (board_state) is a base64 Yjs blob that can exceed the
	// gRPC default 4 MB message limit on large boards, which would fail reads
	// with ResourceExhausted. Raise the max recv/send message size so large
	// boards (and any other big node) load reliably.
	const maxDgraphMsgBytes = 64 * 1024 * 1024 // 64 MB
	dialOpts := append([]grpc.DialOption{},
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.UseCompressor(gzip.Name),
			grpc.MaxCallRecvMsgSize(maxDgraphMsgBytes),
			grpc.MaxCallSendMsgSize(maxDgraphMsgBytes),
		))
	conn, err = grpc.Dial(dgraph.Endpoint, dialOpts...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "dgraphInit/ConnectDgraph Failed to create dgraph client")
		return
	}

	DgraphClient = dgo.NewDgraphClient(api.NewDgraphClient(conn))

	//err = DgraphClient.Login(context.Background(), dgraph.User, dgraph.Password)
	//if err != nil {
	//	helpers.LogErrorWithContext(ctx,"dgraphInit/ConnectDgraph Failed to login to dgraph")
	//	return
	//}

	err = createSchema()
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"dgraphInit/ConnectDgraph failed to create dgraph schema err: %+v",
			err)
	}

	helpers.MessageLogs.InfoLog.Println("Successfully connected with Dgraph server !")

	return
}

func createSchema() (err error) {
	ctx := context.Background()
	op := &api.Operation{}
	op.Schema = `
		user_uuid: string @index(exact) @upsert .
		user_email_id: string @index(exact) @upsert .
		user_created_at: dateTime .
		user_updated_at: dateTime .
		user_deleted_at: dateTime .
		user_posts: [uid] .
		user_channels: [uid] @reverse .
		user_name: string @index(exact) @upsert .
		user_full_name: string @index(trigram) .
		user_job_title: string .
		user_department: string .
		user_app_lang: string .
		user_theme_color: string .
		user_theme_mode: string .
		user_docs: [uid] .
		user_hobbies: string .
		user_device_connected: int .
		user_emoji_statuses: [uid] .
		user_status: string .
		user_profile_object_key: string .
		user_dms: [uid] .
		user_teams: [uid] @reverse .
		user_projects: [uid] .
		user_tasks: [uid] .
		user_events: [uid] .
		user_fav_channels: [uid] @reverse .

		comment_uuid: string @index(exact) @upsert .
		comment_text: string .
		comment_by: uid .
		comment_attachments: [uid] .
		comment_reactions: [uid] @reverse .
		comment_mentions: [uid] .
		comment_post: uid .
		comment_on_content_added_by: uid @reverse .
		comment_chat_grouping_id: string .
		comment_chat: uid .
		comment_replies: [uid] .
		comment_created_at: dateTime .
		comment_updated_at: dateTime .
		comment_deleted_at: dateTime .
		comment_doc: uid .
		comment_board: uid @reverse .
		comment_task: uid .

		ch_moderators: [uid] .
		ch_members: [uid] @reverse .
		ch_name: string @index(trigram) .
		ch_about: string .
		ch_icon: string .
		ch_poster: string .
		ch_posts: [uid] .
		ch_created_at: dateTime .
		ch_updated_at: dateTime .
		ch_deleted_at: dateTime .
		ch_uuid: string @index(exact) @upsert .
		ch_created_by: uid .
		ch_private: bool .
		ch_post_policy: string .
		ch_recording: [uid] .

		status_user_emoji_id: string .
		status_user_emoji_desc: string .
		status_user_emoji_expiry_at: dateTime .
		status_user_emoji_expiry_in: string .

		post_text: string .
		post_by: uid .
		post_created_at: dateTime .
		post_updated_at: dateTime .
		post_deleted_at: dateTime .
		post_comments: [uid] .
		post_attachments: [uid] .
		post_channel: uid .
		post_doc: uid .
		post_mentions: [uid] .
		post_uuid: string @index(exact) @upsert .
		post_reactions: [uid] @reverse .
		post_fwd_msg_post: uid .
		post_fwd_msg_chat: uid .
		post_reply_to: uid .

		dm_grouping_id: string @index(term) .
		dm_participants: [uid] @reverse .
		dm_chats: [uid] .
		dm_recording: [uid] .

		chat_from: uid .
		chat_to: uid .
		chat_created_at: dateTime .
		chat_updated_at: dateTime .
		chat_deleted_at: dateTime .
		chat_body_text: string .
		chat_attachments: [uid] .
		chat_reactions: [uid] @reverse .
		chat_comments: [uid] .
		chat_mentions: [uid] .
		chat_uuid: string @index(exact) @upsert .
		chat_fwd_msg_post: uid .
		chat_fwd_msg_chat: uid .
		chat_reply_to: uid .
		chat_dm: uid .

		attachment_uuid: string @index(exact) .
		attachment_file_name: string .
		attachment_obj_key: string .
		attachment_created_at: dateTime .
		attachment_deleted_at: dateTime .
		attachment_raw_type: string .
		attachment_type: string .
		attachment_width: int .
		attachment_height: int .
		attachment_duration: float .
		attachment_size: int .

		transcript_from: uid .
		transcript_text: string .
		transcript_timestamp: int .
		transcript_offset_ms: int .

		recording_egress_id: string @index(exact) .
		recording_stared_at: dateTime .
		recording_ended_at: dateTime .
		recording_duration: float .
		recording_obj_key: string .
		recording_transcript: [uid] .
		recording_started_by: uid .
		recording_channel: uid .
		recording_dm: uid .
		recording_size: int .
		recording_deleted_at: dateTime .
		recording_transcript_only: bool .

		reaction_added_at: dateTime .
		reaction_emoji_id: string .
		reaction_added_by: uid .
		reaction_on_content_added_by: uid @reverse .

		mention_users: [uid] @reverse .
		mention_chat_uuid: string @index(exact) . 
		mention_post_uuid: string @index(exact) . 
		mention_task_uuid: string @index(exact) . 
		mention_comment_uuid: string @index(exact) . 
		mention_post: uid .
		mention_chat: uid .
		mention_comment: uid .
		mention_created_at:  dateTime .
		mention_updated_at:  dateTime .

		doc_uuid: string @index(exact) @upsert .
		doc_title: string @index(trigram) .
		doc_body: string .
		doc_editing_users: [uid] .
		doc_reading_users: [uid] .
		doc_public_comment: bool .
		doc_private: bool @index(bool) .
		doc_commenting_users: [uid] .
		doc_created_by: uid @reverse .
		doc_comments: [uid] .
		doc_created_at: dateTime .
		doc_updated_at: dateTime .
		doc_deleted_at: dateTime .

		board_uuid: string @index(exact) @upsert .
		board_title: string @index(trigram) .
		board_snippet: string .
		board_thumbnail_key: string .
		board_state: string .
		board_state_key: string .
		board_private: bool @index(bool) .
		board_editing_users: [uid] @reverse .
		board_reading_users: [uid] @reverse .
		board_commenting_users: [uid] @reverse .
		board_created_by: uid @reverse .
		board_created_at: dateTime .
		board_updated_at: dateTime .
		board_deleted_at: dateTime .

		activity_uuid: string @index(exact) @upsert .
		activity_by: uid .
		activity_type: string .
		activity_time: dateTime .
		activity_prev_state: string .
		activity_next_state: string .

		task_uuid: string @index(exact) @upsert .
		task_name: string @index(trigram) .
		task_status: string @index(term) .
		task_custom_status: string @index(exact) .
		task_custom_status_name: string .
		task_label: string .
		task_priority: string @index(term) .
		task_type: string .
		task_sub_tasks: [uid] .
		task_parent_task: uid .
		task_description: string .
		task_assignee: uid @reverse .
		task_google_calendar_id: string .
		task_github_issue_number: int .
		task_github_issue_url: string .
		task_github_pr_number: int .
		task_github_pr_url: string .
		task_github_branch: string .
		task_due_date: dateTime .
		task_start_date: dateTime .
		task_project: uid .
		task_comments: [uid] .
		task_attachments: [uid] .
		task_activities: [uid] .
		task_mentions: [uid] .
		task_created_by: uid .
		task_created_at: dateTime .
		task_updated_at: dateTime .
		task_deleted_at: dateTime .
		task_rank: float .

		event_uuid: string @index(exact) @upsert .
		event_title: string @index(trigram) .
		event_description: string .
		event_start_time: dateTime .
		event_end_time: dateTime .
		event_created_by: uid @reverse .
		event_google_calendar_id: string @index(exact) .
		event_created_at: dateTime .
		event_updated_at: dateTime .
		event_participants: [uid] @reverse .
		event_deleted_at: dateTime .
		event_is_focus: bool .

		team_uuid: string @index(exact) @upsert .
		team_name: string .
		team_members: [uid] @reverse .
		team_admins: [uid] @reverse .
		team_projects: [uid] @reverse .
		team_created_by: uid .
		team_created_at: dateTime .
		team_updated_at: dateTime .
		team_deleted_at: dateTime .
	

		project_uuid: string @index(exact) @upsert .
		project_name: string .
		project_status: string .
		project_tasks: [uid] .
		project_attachments: [uid] .
		project_team: uid .
		project_members: [uid] @reverse .
		project_admins: [uid] @reverse .
		project_created_by: uid .
		project_created_at: dateTime .
		project_updated_at: dateTime .
		project_deleted_at: dateTime .

		# Generic entity links: a task or project can link docs and boards.
		# @reverse lets a doc/board surface the tasks/projects that link it.
		linked_docs: [uid] @reverse .
		linked_boards: [uid] @reverse .

		type Transcript {
			transcript_from: User
			transcript_text
			transcript_timestamp
			transcript_offset_ms
		}

		type Recording {
			recording_egress_id
			recording_stared_at
			recording_ended_at
			recording_duration
			recording_obj_key
			recording_transcript: [Transcript]
			recording_channel: Channel
			recording_dm: Dm
			recording_size
			recording_deleted_at
			recording_transcript_only
		}

		type Doc {
			doc_uuid
			doc_title
			doc_body
			doc_created_by: User
			doc_comments: [Comment]
			doc_private
			doc_public_comment
			doc_editing_users: [User]
			doc_reading_users: [User]
			doc_commenting_users: [User]
			doc_created_at
			doc_updated_at
			doc_deleted_at
		}

		type Board {
			board_uuid
			board_title
			board_snippet
			board_thumbnail_key
			board_state_key
			board_state
			board_created_by: User
			board_private
			board_editing_users: [User]
			board_reading_users: [User]
			board_commenting_users: [User]
			board_created_at
			board_updated_at
			board_deleted_at
		}

		type Status {
			status_user_emoji_id
			status_user_emoji_desc
			status_user_emoji_expiry_at
			status_user_emoji_expiry_in
		}

		type Comment {
			comment_uuid
			comment_text
			comment_attachments: [Attachment]
			comment_reactions: [Reaction]
			comment_mentions: [User]
			comment_post: Post
			comment_doc: Doc
			comment_board: Board
			comment_chat_grouping_id
			comment_chat: Chat
			comment_by : User
			comment_task: Task
			comment_on_content_added_by: User 
			comment_replies: [Comment]
			comment_created_at
			comment_updated_at
			comment_deleted_at
		}

		type Channel {
			ch_uuid
			ch_name
			ch_about
			ch_moderators: [User]
			ch_members: [User]
			ch_posts: [Post]
			ch_icon
			ch_poster
			ch_created_at
			ch_updated_at
			ch_deleted_at
			ch_created_by: User
			ch_private
			ch_post_policy
			ch_recording: [Recording]
		}
		
		type Post {
			post_uuid
			post_text
			post_attachments: [Attachment]
			post_created_at
			post_updated_at
			post_deleted_at
			post_channel: Channel
			post_doc: Doc
			post_comments: [Comment]
			post_mentions: [User]
			post_reactions: [Reaction]
			post_by: User
			post_fwd_msg_post: Post
			post_fwd_msg_chat: Chat
			post_reply_to: Post
		}

		type User {
			user_uuid
			user_name
			user_email_id
			user_app_lang
			user_created_at
			user_updated_at
			user_teams: [Team]
			user_projects: [Project]
			user_tasks: [Task]
			user_events: [Event]
			user_deleted_at
			user_posts: [Post]
			user_channels: [Channel]
			user_docs: [Doc]
			user_full_name
			user_job_title
			user_department
			user_hobbies
			user_profile_object_key
			user_dms: [Dm]
			user_device_connected
			user_emoji_statuses: [Status]
			user_status
		}

		type Attachment {
			attachment_uuid
			attachment_file_name
			attachment_obj_key
			attachment_width
			attachment_height
			attachment_raw_type
			attachment_type
			attachment_duration
			attachment_size
			attachment_created_at
			attachment_deleted_at
		}

		type Chat {
			chat_from: User
			chat_to: User
			chat_created_at
			chat_comments: [Comment]
			chat_updated_at
			chat_deleted_at
			chat_body_text
			chat_reactions: [Reaction]
			chat_attachments: [Attachment]
			chat_uuid
			chat_mentions: [User]
			chat_fwd_msg_post: Post
			chat_fwd_msg_chat: Chat
			chat_reply_to: Chat
			chat_dm: Dm
		}

		type Dm {
			dm_grouping_id
			dm_chats: [Chat]
			dm_participants: [User]
			dm_recording: [Recording]
		}

		type Reaction {
			reaction_added_at
			reaction_emoji_id
			reaction_added_by: User
			reaction_on_content_added_by: User
		}

		type Mention {
			mention_users: [User]
			mention_chat_uuid
			mention_post_uuid
			mention_task_uuid
			mention_comment_uuid
			mention_post: Post
			mention_chat: Chat
			mention_comment: Comment
			mention_created_at
			mention_updated_at
		}

		type Activity {
			activity_uuid
			activity_by: [User]
			activity_type
			activity_time
			activity_prev_state
			activity_next_state
		}

		type Team {
			team_uuid
			team_name
			team_members: [User]
			team_projects: [Project]
			team_admins: [User]
			team_created_by: User
			team_created_at
			team_updated_at
			team_deleted_at
		}

		type Task {
			task_uuid
			task_name
			task_status
			task_custom_status
			task_custom_status_name
			task_description
			task_assignee: User
			task_google_calendar_id: string
			task_github_issue_number
			task_github_issue_url
			task_github_pr_number
			task_github_pr_url
			task_github_branch
			task_due_date
			task_mentions: [User]
			task_start_date
			task_activities: [Activity]
			task_sub_tasks: Task
			task_parent_task: Task
			task_type
			task_project: Project
			task_comments: [Comment]
			task_attachments: [Attachment]
			task_created_by: User
			task_label
			task_priority
			task_created_at
			task_updated_at
			task_deleted_at
			task_rank
			linked_docs: [Doc]
			linked_boards: [Board]
		}

		type Event {
			event_uuid
			event_title
			event_description
			event_start_time
			event_end_time
			event_created_by: User
			event_google_calendar_id
			event_is_focus
			event_participants: [User]
			event_created_at
			event_updated_at
			event_deleted_at
		}

		type Project {
			project_uuid
			project_name
			project_status
			project_tasks: [Task]
			project_attachments: [Attachment]
			project_team: Team
			project_members: [User]
			project_admins: [User]
			project_created_by: User
			project_created_at
			project_updated_at
			project_deleted_at
			linked_docs: [Doc]
			linked_boards: [Board]
		}

		mem_uuid: string @index(exact) @upsert .
		mem_kind: string @index(exact) .
		mem_content: string .
		mem_status: string @index(exact) .
		mem_confidence: int .
		mem_due_at: dateTime @index(hour) .
		mem_grp_id: string @index(exact) .
		mem_created_at: dateTime .
		mem_updated_at: dateTime .
		mem_owner: uid @reverse .
		mem_channel: uid @reverse .
		mem_project: uid @reverse .

		type MemoryItem {
			mem_uuid
			mem_kind
			mem_content
			mem_status
			mem_confidence
			mem_due_at
			mem_grp_id
			mem_created_at
			mem_updated_at
			mem_owner: User
			mem_channel: Channel
			mem_project: Project
		}

	`
	err = DgraphClient.Alter(ctx, op)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"dgraphInit/createSchema failed to create dgraph schema err: %+v",
			err)
	}

	return
}
