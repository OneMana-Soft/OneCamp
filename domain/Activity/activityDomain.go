package domain

import (
	"context"
	"strconv"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
)

const MENTION_COUNT = 10
const COMMENT_COUNT = 10
const REACTION_COUNT = 10

func GetLatestCommentActivityByUserId(ctx context.Context, userDgraphId string, pageIndex int, pageSize int) (dgraphComments []*dgraphStruct.DgraphComment, actualLen int, err error) {

	offset := pageIndex * pageSize
	firstVal := strconv.Itoa(pageSize + 1)
	offsetVal := strconv.Itoa(offset)

	variables := make(map[string]string)

	variables["$user_id"] = userDgraphId
	variables["$first"] = firstVal
	variables["$offset"] = offsetVal
	query := `query CommentInfo($user_id: string, $first: int, $offset: int) {
				commentInfo(func: uid($user_id)) {
					comments: ~comment_on_content_added_by @filter(
					not gt(comment_deleted_at, "1970-01-01T00:00:00Z")
						AND (
							has(comment_doc) 
							OR has(comment_post) 
							OR has(comment_chat) 
							OR has(comment_task)
							OR has(comment_board)
						)
						AND NOT uid_in(comment_by, $user_id)
					) 
					(orderdesc: comment_created_at, first: $first, offset: $offset){

						comment_text
						comment_by {
							user_uuid
							user_profile_object_key
							user_full_name
						}
						comment_created_at
						comment_post @filter(not gt(post_deleted_at, "1970-01-01T00:00:00Z")) @cascade(post_channel) {
							post_uuid
							post_text
							post_created_at
							post_by {
								user_uuid
								user_name
								user_profile_object_key
								user_full_name
							}
							post_channel @filter(uid_in(ch_members, $user_id)){
								ch_name
								ch_uuid
							}
						}
						comment_chat @filter(not gt(chat_deleted_at, "1970-01-01T00:00:00Z")) @cascade(chat_dm) {
							chat_uuid
							chat_dm  @filter(uid_in(dm_participants, $user_id)){
								dm_grouping_id
							}
							chat_from {
								uid
								user_uuid
								user_profile_object_key
								user_full_name
							}
							chat_created_at
							chat_body_text
						}

						comment_task @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z")) @cascade(task_project) {

							task_uuid
							task_name
							task_project @filter(uid_in(project_members, $user_id)) {
								project_name
								project_uuid
							}
						}

						comment_doc @filter(not gt(doc_deleted_at, "1970-01-01T00:00:00Z")) {
							doc_uuid
							doc_title

						}
						comment_board @filter(not gt(board_deleted_at, "1970-01-01T00:00:00Z")) {
							board_uuid
							board_title
						}
						
					}
				}
			}`

	dgraphComments, actualLen, err = dgraphModels.GetComments(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetLatestCommentActivityByUserId Failed to get comment in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetLatestMentionByUserId(ctx context.Context, userDgraphId string, pageIndex int, pageSize int) (dgraphMentions []*dgraphStruct.DgraphMentions, actualLen int, err error) {

	offset := pageIndex * pageSize
	firstVal := strconv.Itoa(pageSize + 1)
	offsetVal := strconv.Itoa(offset)

	variables := make(map[string]string)
	variables["$user_id"] = userDgraphId
	variables["$first"] = firstVal
	variables["$offset"] = offsetVal

	query := `query MentionInfo($user_id: string, $first: int, $offset: int) {
				mentionInfo(func: uid($user_id)) {
					mentions: ~mention_users @filter(
						has(mention_chat) 
						OR has(mention_post) 
						OR has(mention_comment) 

					)(orderdesc: mention_created_at, first: $first, offset: $offset) {
						mention_created_at
						mention_updated_at
						mention_chat @filter(not gt(chat_deleted_at, "1970-01-01T00:00:00Z") AND NOT uid_in(chat_from, $user_id)) @cascade(chat_dm) {
							chat_uuid
							chat_from {
								uid
								user_uuid
								user_profile_object_key
								user_full_name
							}
							
							chat_dm @filter(uid_in(dm_participants, $user_id)){
								dm_grouping_id
							}
							chat_created_at
							chat_body_text
						}

						mention_task @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z")) @cascade(task_project){

							task_uuid
							task_name
							task_description
							task_project @filter(uid_in(project_members, $user_id)) {
								project_name
								project_uuid
							}
						}
						mention_post @filter(not gt(post_deleted_at, "1970-01-01T00:00:00Z") AND NOT uid_in(post_by, $user_id)) @cascade(post_channel) {
							post_uuid
							post_text
							post_created_at
							post_by {
								user_uuid
								user_name
								user_profile_object_key
								user_full_name
							}
							post_channel @filter(uid_in(ch_members, $user_id)) {
								ch_name
								ch_uuid
							}
						}

						mention_comment @filter(not 
							gt(comment_deleted_at, "1970-01-01T00:00:00Z")
							AND (
								has(comment_doc) 
								OR has(comment_post) 
								OR has(comment_chat) 
								OR has(comment_task)
								OR has(comment_board)
							)
							AND NOT uid_in(comment_by, $user_id)
						) {
							comment_post @filter(not gt(post_deleted_at, "1970-01-01T00:00:00Z")) @cascade(post_channel) {
								post_uuid
								post_channel @filter(uid_in(ch_members, $user_id)){
									ch_name
									ch_uuid
								}
							}
							comment_chat @filter(not gt(chat_deleted_at, "1970-01-01T00:00:00Z")) @cascade(chat_dm) {
								chat_uuid
								chat_from {
									user_uuid
									user_profile_object_key
									user_full_name
								}
								chat_dm @filter(uid_in(dm_participants, $user_id)){
									dm_grouping_id
								}
							}

							comment_task @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z")) @cascade(task_project) {

								task_uuid
								task_name
								task_project @filter(uid_in(project_members, $user_id)) {
									project_name
									project_uuid
								}
							}

							comment_doc @filter(not gt(doc_deleted_at, "1970-01-01T00:00:00Z")) {
								doc_uuid
								doc_title

							}
							comment_board @filter(not gt(board_deleted_at, "1970-01-01T00:00:00Z")) {
								board_uuid
								board_title
							}
							comment_text
							comment_by {
								user_uuid
								user_profile_object_key
								user_full_name
							}
							comment_created_at
						}
					}
			  	}
			}`

	dgraphMentions, actualLen, err = dgraphModels.GetMentions(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetUserChannelListWithLatestPost Failed to get mention in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetReactionsByUserId(ctx context.Context, userDgraphId string, pageIndex int, pageSize int) (dgraphReaction []*dgraphModels.ReactionsActivity, actualLen int, err error) {

	offset := pageIndex * pageSize
	firstVal := strconv.Itoa(pageSize + 1)
	offsetVal := strconv.Itoa(offset)

	variables := make(map[string]string)
	variables["$user_id"] = userDgraphId
	variables["$first"] = firstVal
	variables["$offset"] = offsetVal

	query := `query ReactionInfo($user_id: string, $first: int, $offset: int){
				reactionInfo(func: uid($user_id)) {
					reactions: ~reaction_on_content_added_by @filter(
						(has(~post_reactions) 
						OR has(~chat_reactions) 
						OR has(~comment_reactions))
						AND NOT uid_in(reaction_added_by, $user_id)
					)(orderdesc: reaction_added_at, first: $first, offset: $offset) {
						reaction_added_at
						reaction_emoji_id
						reaction_added_by {
							user_uuid
							user_name
							user_profile_object_key
							user_full_name
						}
						post: ~post_reactions @filter(not gt(post_deleted_at, "1970-01-01T00:00:00Z")) @cascade(post_channel) {
							post_uuid
							post_text
							post_created_at
							post_by {
								user_uuid
								user_name
								user_profile_object_key
								user_full_name
							}
							post_channel @filter(uid_in(ch_members, $user_id)) {
								ch_name
								ch_uuid
							}
						}

						chat: ~chat_reactions @filter(not gt(chat_deleted_at, "1970-01-01T00:00:00Z")) @cascade(chat_dm) {
							chat_uuid
							chat_from {
								user_uuid
								user_profile_object_key
								user_full_name
							}
							chat_dm @filter(uid_in(dm_participants, $user_id)){
								dm_grouping_id
							}
							chat_body_text
						}

						comment: ~comment_reactions @filter(
							not gt(comment_deleted_at, "1970-01-01T00:00:00Z")
							AND(
								has(comment_post) 
								OR has(comment_chat) 
								OR has(comment_doc) 
								OR has(comment_task) 
							)
							
							) 
						{
							comment_post @filter(not gt(post_deleted_at, "1970-01-01T00:00:00Z")) @cascade(post_channel) {
								post_uuid
								post_channel @filter(uid_in(ch_members, $user_id)){
									ch_name
									ch_uuid
								}
							}
							comment_chat @filter(not gt(chat_deleted_at, "1970-01-01T00:00:00Z")) @cascade(chat_dm) {
								chat_uuid
								chat_from {
									user_uuid
									user_profile_object_key
									user_full_name
								}
								chat_dm @filter(uid_in(dm_participants, $user_id)){
									dm_grouping_id
								}
							}

							comment_task @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z")) @cascade(task_project) {

								task_uuid
								task_name
								task_project @filter(uid_in(project_members, $user_id)) {
									project_name
									project_uuid
								}
							}
							
							comment_doc @filter(not gt(doc_deleted_at, "1970-01-01T00:00:00Z")) {
								doc_uuid
								doc_title

							}
							comment_text
							comment_by {
								user_uuid
								user_profile_object_key
								user_full_name
							}
							comment_created_at
						}
					}
			  	}
			}`

	dgraphReaction, actualLen, err = dgraphModels.GetReactions(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetReactions Failed to get reation in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetUnifiedActivity(ctx context.Context, userDgraphId string, beforeTime time.Time, limit int) (
	mentions []*dgraphStruct.DgraphMentions,
	comments []*dgraphStruct.DgraphComment,
	reactions []*dgraphModels.ReactionsActivity,
	err error,
) {

	variables := make(map[string]string)
	variables["$user_id"] = userDgraphId
	variables["$limit"] = strconv.Itoa(limit + 1)
	variables["$time"] = beforeTime.Format(time.RFC3339)

	query := `query UnifiedActivity($user_id: string, $limit: int, $time: string) {
		mentionInfo(func: uid($user_id)) {
			mentions: ~mention_users @filter(lt(mention_created_at, $time) AND (has(mention_chat) OR has(mention_post) OR has(mention_comment))) (orderdesc: mention_created_at, first: $limit) {
				mention_created_at
				mention_updated_at
				mention_chat @filter(not gt(chat_deleted_at, "1970-01-01T00:00:00Z") AND NOT uid_in(chat_from, $user_id)) @cascade(chat_dm) {
					chat_uuid
					chat_from {
						uid
						user_uuid
						user_profile_object_key
						user_full_name
						is_bot
						user_email_id
					}
					chat_dm @filter(uid_in(dm_participants, $user_id)){
						dm_grouping_id
					}
					chat_created_at
					chat_body_text
				}

				mention_task @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z")) @cascade(task_project){

					task_uuid
					task_name
					task_description
					task_project @filter(uid_in(project_members, $user_id)) {
						project_name
						project_uuid
					}
				}

				mention_post @filter(not gt(post_deleted_at, "1970-01-01T00:00:00Z") AND NOT uid_in(post_by, $user_id)) @cascade(post_channel) {
					post_uuid
					post_text
					post_created_at
					post_by {
						user_uuid
						user_name
						user_profile_object_key
						user_full_name
						is_bot
						user_email_id
					}
					post_channel @filter(uid_in(ch_members, $user_id)) {
						ch_name
						ch_uuid
					}
				}
				mention_comment @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z") AND (has(comment_doc) OR has(comment_post) OR has(comment_chat) OR has(comment_task) OR has(comment_board)) AND NOT uid_in(comment_by, $user_id)) {
					comment_post @filter(not gt(post_deleted_at, "1970-01-01T00:00:00Z")) @cascade(post_channel) {
						post_uuid
						post_channel @filter(uid_in(ch_members, $user_id)){
							ch_name
							ch_uuid
						}
					}
					comment_chat @filter(not gt(chat_deleted_at, "1970-01-01T00:00:00Z")) @cascade(chat_dm) {
						chat_uuid
						chat_from {
							user_uuid
							user_profile_object_key
							user_full_name
						}
						chat_dm @filter(uid_in(dm_participants, $user_id)){
							dm_grouping_id
						}
					}
					comment_task @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z")) @cascade(task_project) {
						task_uuid
						task_name
						task_project @filter(uid_in(project_members, $user_id)) {
							project_name
							project_uuid
						}
					}
					comment_doc @filter(not gt(doc_deleted_at, "1970-01-01T00:00:00Z")) {
						doc_uuid
						doc_title
					}
					comment_board @filter(not gt(board_deleted_at, "1970-01-01T00:00:00Z")) {
						board_uuid
						board_title
					}
					comment_text
					comment_by {
						user_uuid
						user_profile_object_key
						user_full_name
						is_bot
						user_email_id
					}
					comment_created_at
				}
			}
		}

		commentInfo(func: uid($user_id)) {
			comments: ~comment_on_content_added_by @filter(lt(comment_created_at, $time) AND not gt(comment_deleted_at, "1970-01-01T00:00:00Z") AND (has(comment_doc) OR has(comment_post) OR has(comment_chat) OR has(comment_task) OR has(comment_board)) AND NOT uid_in(comment_by, $user_id)) (orderdesc: comment_created_at, first: $limit){
				comment_text
				comment_by {
					user_uuid
					user_profile_object_key
					user_full_name
					is_bot
					user_email_id
				}
				comment_created_at
				comment_post @filter(not gt(post_deleted_at, "1970-01-01T00:00:00Z")) @cascade(post_channel) {
					post_uuid
					post_text
					post_created_at
					post_by {
						user_uuid
						user_name
						user_profile_object_key
						user_full_name
					}
					post_channel @filter(uid_in(ch_members, $user_id)){
						ch_name
						ch_uuid
					}
				}
				comment_chat @filter(not gt(chat_deleted_at, "1970-01-01T00:00:00Z")) @cascade(chat_dm) {
					chat_uuid
					chat_dm  @filter(uid_in(dm_participants, $user_id)){
						dm_grouping_id
					}
					chat_from {
						uid
						user_uuid
						user_profile_object_key
						user_full_name
					}
					chat_created_at
					chat_body_text
				}
				comment_task @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z")) @cascade(task_project) {
					task_uuid
					task_name
					task_project @filter(uid_in(project_members, $user_id)) {
						project_name
						project_uuid
					}
				}
				comment_doc @filter(not gt(doc_deleted_at, "1970-01-01T00:00:00Z")) {
					doc_uuid
					doc_title
				}
				comment_board @filter(not gt(board_deleted_at, "1970-01-01T00:00:00Z")) {
					board_uuid
					board_title
				}
			}
		}

		reactionInfo(func: uid($user_id)) {
			reactions: ~reaction_on_content_added_by @filter(lt(reaction_added_at, $time) AND (has(~post_reactions) OR has(~chat_reactions) OR has(~comment_reactions)) AND NOT uid_in(reaction_added_by, $user_id)) (orderdesc: reaction_added_at, first: $limit) {
				reaction_added_at
				reaction_emoji_id
				reaction_added_by {
					user_uuid
					user_name
					user_profile_object_key
					user_full_name
					is_bot
					user_email_id
				}
				post: ~post_reactions @filter(not gt(post_deleted_at, "1970-01-01T00:00:00Z")) @cascade(post_channel) {
					post_uuid
					post_text
					post_created_at
					post_by {
						user_uuid
						user_name
						user_profile_object_key
						user_full_name
					}
					post_channel @filter(uid_in(ch_members, $user_id)) {
						ch_name
						ch_uuid
					}
				}
				chat: ~chat_reactions @filter(not gt(chat_deleted_at, "1970-01-01T00:00:00Z")) @cascade(chat_dm) {
					chat_uuid
					chat_from {
						user_uuid
						user_profile_object_key
						user_full_name
					}
					chat_dm @filter(uid_in(dm_participants, $user_id)){
						dm_grouping_id
					}
					chat_body_text
				}
				comment: ~comment_reactions @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z") AND(has(comment_post) OR has(comment_chat) OR has(comment_doc) OR has(comment_task))) {
					comment_post @filter(not gt(post_deleted_at, "1970-01-01T00:00:00Z")) @cascade(post_channel) {
						post_uuid
						post_channel @filter(uid_in(ch_members, $user_id)){
							ch_name
							ch_uuid
						}
					}
					comment_chat @filter(not gt(chat_deleted_at, "1970-01-01T00:00:00Z")) @cascade(chat_dm) {
						chat_uuid
						chat_from {
							user_uuid
							user_profile_object_key
							user_full_name
						}
						chat_dm @filter(uid_in(dm_participants, $user_id)){
							dm_grouping_id
						}
					}
					comment_task @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z")) @cascade(task_project) {
						task_uuid
						task_name
						task_project @filter(uid_in(project_members, $user_id)) {
							project_name
							project_uuid
						}
					}
					comment_doc @filter(not gt(doc_deleted_at, "1970-01-01T00:00:00Z")) {
						doc_uuid
						doc_title
					}
					comment_text
					comment_by {
						user_uuid
						user_profile_object_key
						user_full_name
					}
					comment_created_at
				}
			}
		}
	}`

	mentions, comments, reactions, err = dgraphModels.GetUnifiedActivity(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetUnifiedActivity Failed to get unified activity in dgraph err: %+v",
			err,
		)
		return

	}

	return
}
