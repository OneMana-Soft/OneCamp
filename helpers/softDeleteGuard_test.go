package helpers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The owner's rule (V-184, from V-179): every delete of a business record is
// soft. A business record is anything a person made, owns, shares or grants,
// or that an audit would ask about. Its delete marks the row (deleted_at,
// deleted_by), every read sees only live rows, and a restore brings it back
// (models/postgres/SoftDelete).
//
// A hard DELETE FROM is right only for rows that are not records: a failed
// write's own half-written row, rows past their retention, credentials that
// must not outlive their revocation, rows rebuilt from records kept
// elsewhere, a property of a record that stays, a person's own toggle or
// setting, and live state that ends by design. TestEveryHardDeleteIsReasoned
// reads every string literal in the repository's non-test Go and fails on a
// DELETE FROM that isn't listed in hardDeletes with its reason, and on an
// entry that matches nothing (so a moved site takes its entry with it).
//
// A hard delete of something an audit asks about stands only when the change
// is written to the admin audit chain: a log line is not a record
// (CODE-REVIEWER-150, row 2220). Without that, it is TO-SOFT.
//
// The TO-SOFT entries are business records still deleted for good: V-184's
// later heads move them to soft deletes, red first, and each removes its
// entries and lowers toSoftBaseline. New business records are soft from the
// start, so the baseline only falls.

// hardDeleteClasses are the reasons a hard delete may give, each a prefix of
// an entry's reason.
var hardDeleteClasses = []string{
	"rollback: ",  // a failed write's own half-written row, taken back
	"purge: ",     // rows past their retention or trash window, or a purged record's own rows
	"secret: ",    // a credential or token, which must be gone once revoked or used
	"derived: ",   // an index, queue, mirror or bookkeeping row rebuilt from records kept elsewhere
	"edit: ",      // a property of a record that stays (a task's cycle, a field's value)
	"toggle: ",    // a person's own on/off: a follow, a pin, a reaction, a vote, a saved item
	"setting: ",   // a setting back to its default
	"ephemeral: ", // live state that ends by design: a call's admissions, a draft, an unclaimed upload
	"demo: ",      // the demo's reset of its own visitor's rows
	"TO-SOFT: ",   // a business record still deleted for good: V-184 moves it
}

// toSoftBaseline is how many TO-SOFT entries hardDeletes holds. A head that
// moves one to a soft delete removes its entry and lowers this.
const toSoftBaseline = 14 // 20 until V-184 head 3 kept project roles (Delete, DeleteIn, Keep); 17 until head 3b kept calendar shares and admin roles; 15 until head 3c kept withdrawn invitations

// hardDeletes is every hard DELETE FROM in non-test Go, keyed
// "path func table" (with " xN" when a function deletes from a table N
// times), and why it may stay hard.
var hardDeletes = map[string]string{
	"business/Chat/durable.go takeBackChat chats":                                   "rollback: a chat whose graph write failed is taken back with its message and conversation rows",
	"business/Chat/durable.go takeBackChat conversations":                           "rollback: a chat whose graph write failed is taken back with its message and conversation rows",
	"business/Chat/durable.go takeBackChat messages":                                "rollback: a chat whose graph write failed is taken back with its message and conversation rows",
	"business/Command/appBusiness.go DeleteApp integrations":                        "secret: an integration's encrypted credentials, gone once it is disconnected",
	"business/Command/appOAuth.go DisconnectOAuthApp integrations":                  "secret: an integration's encrypted credentials, gone once it is disconnected",
	"business/Connector/connectorBusiness.go Disconnect integrations":               "secret: an integration's encrypted credentials, gone once it is disconnected",
	"business/Doc/docTrash.go purgeDocRows doc_snapshots":                           "purge: a doc purged from the trash takes its own snapshots, views, saves and grants",
	"business/Doc/docTrash.go purgeDocRows guest_grants":                            "purge: a doc purged from the trash takes its own snapshots, views, saves and grants",
	"business/Doc/docTrash.go purgeDocRows resource_views":                          "purge: a doc purged from the trash takes its own snapshots, views, saves and grants",
	"business/Doc/docTrash.go purgeDocRows saved_items":                             "purge: a doc purged from the trash takes its own snapshots, views, saves and grants",
	"business/Post/durable.go takeBackPost messages":                                "rollback: a post whose graph write failed is taken back with its message row",
	"business/Post/durable.go takeBackPost posts":                                   "rollback: a post whose graph write failed is taken back with its message row",
	"domain/Attachment/attachmentPurge.go DeleteArchivedAttachmentRows attachments": "purge: attachment rows archived past their retention",
	"domain/Channel/channelDomain.go HardDeleteChannel channels":                    "rollback: compensation for a create whose graph write failed (CompensateOnFailure); a kept row would hold its unique name",
	"domain/Chat/chatDomain.go HardDeleteChatByUUID chats":                          "rollback: compensation for a create whose graph write failed (CompensateOnFailure); a kept row would hold its unique name",
	"domain/Comment/commentDomain.go HardDeleteCommentByUUID comments":              "rollback: compensation for a create whose graph write failed (CompensateOnFailure); a kept row would hold its unique name",
	"domain/Integration/integrationDomain.go DeleteIntegration integrations":        "TO-SOFT: the org's GitHub integration, disconnected (DisconnectGitHub): keep who disconnected it and when; its soft delete clears the credentials",
	"domain/Post/postDomain.go HardDeletePostByUUIUD posts":                         "rollback: compensation for a create whose graph write failed (CompensateOnFailure); a kept row would hold its unique name",
	"domain/Project/projectDomain.go HardDeleteProject projects":                    "rollback: compensation for a create whose graph write failed (CompensateOnFailure); a kept row would hold its unique name",
	"domain/Team/teamDomain.go HardDeleteTeam teams":                                "rollback: compensation for a create whose graph write failed (CompensateOnFailure); a kept row would hold its unique name",
	"domain/User/userDomian.go HardDeleteUser users":                                "rollback: compensation for a create whose graph write failed (CompensateOnFailure); a kept row would hold its unique name",
	"domain/UserChannelNotification/userChannelNotificationDomain.go DeleteNotificationTypeWhenUserIsRemovedFormChannel users_channel_notification": "setting: a removed member's notification setting for that place",
	"domain/UserChatNotification/userChatNotificationDomain.go ForgetChatSetting users_chat_notification":                                           "setting: the chat notification settings of people who left the chat",
	"domain/UserChatNotification/userChatNotificationDomain.go ForgetLeaversChatSettings users_chat_notification":                                   "setting: the chat notification settings of people who left the chat",
	"domain/UserFCMToken/userFCMTokenDomain.go DeleteByUserId users_fcm_token":                                                                      "secret: push device tokens, signed out or rejected by the push service",
	"domain/UserFCMToken/userFCMTokenDomain.go DeleteByUserIdAndDeviceId users_fcm_token":                                                           "secret: push device tokens, signed out or rejected by the push service",
	"domain/UserFCMToken/userFCMTokenDomain.go DeleteFromListOfFCMToken users_fcm_token":                                                            "secret: push device tokens, signed out or rejected by the push service",
	"domain/UserProjectNotification/userProjectNotificationDomain.go DeleteNotificationTypeWhenUserIsRemovedFormProject users_project_notification": "setting: a removed member's notification setting for that place",
	"domain/Webhook/webhookDomain.go CleanupOldWebhookLogs webhook_logs":                                                                            "purge: webhook logs past their retention",
	"models/postgres/AI/aiConfigModel.go DeleteCustomProvider ai_providers":                                                                         "TO-SOFT: a custom AI provider an admin added: its soft delete clears the key and keeps which endpoint and models the workspace used",
	"models/postgres/AI/authorizedModelModel.go DeleteAuthorizedModel ai_authorized_models":                                                         "TO-SOFT: an AI model an admin authorized: an audit asks who allowed what and when",
	"models/postgres/AIAgent/aiAgentEvalModel.go pruneEvalResults ai_agent_eval_results":                                                            "purge: an eval scenario keeps only its newest results",
	"models/postgres/Access/accessModel.go ClearAllExpiry access_grant_expiry":                                                                      "derived: a grant's expiry schedule; the grant keeps its own record",
	"models/postgres/Access/accessModel.go ClearExpiry access_grant_expiry":                                                                         "derived: a grant's expiry schedule; the grant keeps its own record",
	"models/postgres/Access/accessModel.go Ended access_grant_expiry":                                                                               "derived: a grant's expiry schedule; the grant keeps its own record",
	"models/postgres/Approvals/model.go DeleteRule status_rules":                                                                                    "TO-SOFT: an automation rule a person made on a status",
	"models/postgres/BoardSnapshot/boardSnapshotModel.go DeleteByIDs board_snapshots":                                                               "purge: board snapshots past what a board keeps",
	"models/postgres/BoardSnapshot/boardSnapshotModel.go PruneBoardSnapshots board_snapshots":                                                       "purge: board snapshots past what a board keeps",
	"models/postgres/Booking/bookingModel.go Forget bookings":                                                                                       "rollback: a booking whose meeting couldn't be made, so it doesn't hold the slot",
	"models/postgres/BulkPostAndChatModel/bulkPostAndChatModel.go BulkDeleteChatsAndPosts chats":                                                    "rollback: a forward's posts and chats taken back when its graph write failed",
	"models/postgres/BulkPostAndChatModel/bulkPostAndChatModel.go BulkDeleteChatsAndPosts posts":                                                    "rollback: a forward's posts and chats taken back when its graph write failed",
	"models/postgres/CalendarV2/answerModel.go ClearInstanceAnswers event_instance_answers":                                                         "edit: a guest's per-instance answers, reset when the series changes",
	"models/postgres/CalendarV2/answerModel.go ForgetInstanceAnswers event_instance_answers":                                                        "edit: a guest's per-instance answers, reset when the series changes",
	"models/postgres/CalendarV2/attendeeModel.go RemoveAttendees event_attendees":                                                                   "edit: an event's guest list",
	"models/postgres/CalendarV2/attendeeModel.go RemoveEmailGuests event_attendees":                                                                 "edit: an event's guest list",
	"models/postgres/CalendarV2/calendarModel.go DeleteSubscribedCalendar calendars":                                                                "TO-SOFT: a calendar subscription a person added",
	"models/postgres/CalendarV2/calendarModel.go DeleteSubscribedEvents events":                                                                     "derived: a subscribed calendar's events, mirrored from its feed",
	"models/postgres/CalendarV2/calendarModel.go ReplaceMovedAttendees event_attendees":                                                             "edit: an event's guest list",
	"models/postgres/CalendarV2/eventModel.go RemoveFoldedOverrides events":                                                                         "edit: a recurring event's overrides folded back into its series",
	"models/postgres/CalendarV2/oooModel.go SetOOORule event_ooo_rules":                                                                             "setting: an out-of-office event's decline rule back to none",
	"models/postgres/CalendarV2/reminderModel.go SetReminders event_reminders":                                                                      "setting: a person's reminders on an event, replaced as a set",
	"models/postgres/CalendarV2/syncModel.go DropSubscription calendar_sync_calendars":                                                              "derived: calendar sync bookkeeping mirrored from the provider",
	"models/postgres/CalendarV2/syncModel.go DropSubscription calendar_sync_refs":                                                                   "derived: calendar sync bookkeeping mirrored from the provider",
	"models/postgres/CalendarV2/syncModel.go DropSyncRefs calendar_sync_refs":                                                                       "derived: calendar sync bookkeeping mirrored from the provider",
	"models/postgres/CalendarV2/syncModel.go DropSyncRefs calendar_sync_refusals":                                                                   "derived: calendar sync bookkeeping mirrored from the provider",
	"models/postgres/CalendarV2/syncModel.go KeepSyncRefs calendar_sync_refusals":                                                                   "derived: calendar sync bookkeeping mirrored from the provider",
	"models/postgres/CallQA/callQAModel.go Upvote call_question_votes":                                                                              "toggle: a person's upvote on a call question",
	"models/postgres/CallQuality/qualityModel.go Purge call_quality_samples":                                                                        "purge: call quality samples past their retention",
	"models/postgres/CallState/admissionModel.go ForgetAdmissionOf call_admissions":                                                                 "ephemeral: admissions to a call that has ended",
	"models/postgres/CallState/admissionModel.go ForgetAdmissionsTo call_admissions":                                                                "ephemeral: admissions to a call that has ended",
	"models/postgres/CallState/breakoutModel.go SetBreakouts call_breakouts":                                                                        "ephemeral: a live call's breakout rooms and assignments",
	"models/postgres/CallState/breakoutModel.go Unassign call_breakout_assignments":                                                                 "ephemeral: a live call's breakout rooms and assignments",
	"models/postgres/Config/cspViolationModel.go ClearCSPViolations csp_violations":                                                                 "purge: CSP violation reports, telemetry an admin clears",
	"models/postgres/Config/scopedPolicy.go ClearScopedValue policy_scoped_values":                                                                  "setting: a scoped policy value back to its default",
	"models/postgres/ContentIO/jobModel.go DeleteFinishedBefore content_io_jobs":                                                                    "purge: finished import and export jobs past their retention",
	"models/postgres/Conversation/ledger.go ReleaseCopy messages":                                                                                   "rollback: a share's claim taken back when its copy couldn't be written",
	"models/postgres/Cycle/cycleModel.go Complete task_cycles":                                                                                      "edit: a task's cycle; the task keeps the change in its history",
	"models/postgres/Cycle/cycleModel.go SetTasks task_cycles":                                                                                      "edit: a task's cycle; the task keeps the change in its history",
	"models/postgres/Cycle/schedule.go DropSchedule cycle_schedules":                                                                                "setting: a project's cycle schedule turned off",
	"models/postgres/DataTable/cellIndexModel.go ReplaceCellIndex data_table_cell_index":                                                            "derived: the cell search index, rebuilt from the rows",
	"models/postgres/DataTable/cellIndexModel.go ReplaceCells data_table_cell_index":                                                                "derived: the cell search index, rebuilt from the rows",
	"models/postgres/DataTable/computedModel.go AnswerDirty data_table_computed_dirty":                                                              "derived: the computed-column work queue",
	"models/postgres/DataTable/computedModel.go MarkComputedFilled data_table_computed_dirty":                                                       "derived: the computed-column work queue",
	"models/postgres/DataTable/ledger.go PurgeHistory data_table_change_sets":                                                                       "purge: table change history past its retention",
	"models/postgres/DataTable/ledger.go PurgeTrash data_table_changes":                                                                             "purge: table rows and fields past the trash window, with their links and changes",
	"models/postgres/DataTable/ledger.go PurgeTrash data_table_fields":                                                                              "purge: table rows and fields past the trash window, with their links and changes",
	"models/postgres/DataTable/ledger.go PurgeTrash data_table_links":                                                                               "purge: table rows and fields past the trash window, with their links and changes",
	"models/postgres/DataTable/ledger.go PurgeTrash data_table_rows":                                                                                "purge: table rows and fields past the trash window, with their links and changes",
	"models/postgres/DataTable/ledger.go removeLinks data_table_links":                                                                              "edit: a row's links to other rows; the table's change ledger keeps each change for undo",
	"models/postgres/DataTable/ledger.go takeRowLinks data_table_links":                                                                             "edit: a row's links to other rows; the table's change ledger keeps each change for undo",
	"models/postgres/DataTable/purge.go DeleteTablesForGood data_table_changes":                                                                     "purge: tables whose purge time came (purge_at), with their rows and changes",
	"models/postgres/DataTable/purge.go DeleteTablesForGood data_table_rows":                                                                        "purge: tables whose purge time came (purge_at), with their rows and changes",
	"models/postgres/DataTable/purge.go DeleteTablesForGood data_tables":                                                                            "purge: tables whose purge time came (purge_at), with their rows and changes",
	"models/postgres/DocExport/consentModel.go Withdraw doc_export_consents":                                                                        "TO-SOFT: a consent to export, withdrawn: an audit asks when it was given and withdrawn",
	"models/postgres/DocFile/docFileModel.go DropUnseen attachments":                                                                                "rollback: a duplicate's file versions taken back for copies whose nodes were never made",
	"models/postgres/DocFile/docFileModel.go DropUnseen doc_file_versions":                                                                          "rollback: a duplicate's file versions taken back for copies whose nodes were never made",
	"models/postgres/DocFile/docFileModel.go ForgetFiles doc_file_versions":                                                                         "purge: the file versions of docs purged for good",
	"models/postgres/DocSnapshot/docSnapshotModel.go DeleteUnkeptByIDs doc_snapshots":                                                               "purge: doc snapshots the keep rule dropped",
	"models/postgres/EntityLink/docTaskLinks.go RemoveDocTask doc_task_links":                                                                       "edit: a doc's linked tasks",
	"models/postgres/EstimateScale/estimateScaleModel.go Clear project_estimate_scales":                                                             "setting: a project's estimate scale back to points",
	"models/postgres/Follow/followModel.go ClearFollow resource_follows":                                                                            "toggle: a person's follow of a resource",
	"models/postgres/Follow/followModel.go forgetResources resource_follows":                                                                        "purge: a purged resource's follows, notices and mentions",
	"models/postgres/Follow/followModel.go forgetResources resource_mention_syncs":                                                                  "purge: a purged resource's follows, notices and mentions",
	"models/postgres/Follow/followModel.go forgetResources resource_mentions":                                                                       "purge: a purged resource's follows, notices and mentions",
	"models/postgres/Follow/followModel.go forgetResources resource_notices":                                                                        "purge: a purged resource's follows, notices and mentions",
	"models/postgres/Follow/mentionModel.go SyncMentions resource_mentions":                                                                         "derived: mentions synced from a doc's body",
	"models/postgres/Follow/noticeModel.go DeleteNoticesBefore resource_notices":                                                                    "purge: notices past their retention",
	"models/postgres/Form/formModel.go ForgetOfTables project_forms":                                                                                "purge: the forms of tables purged for good",
	"models/postgres/Form/uploadModel.go Forget form_uploads":                                                                                       "ephemeral: form uploads never claimed by a submission, past their window",
	"models/postgres/Form/uploadModel.go Release attachments":                                                                                       "ephemeral: an unclaimed form upload's attachment row, released",
	"models/postgres/GitHubImportJob/githubImportJobModel.go CleanupOld github_import_jobs":                                                         "purge: GitHub import jobs past their retention",
	"models/postgres/Goal/goalModel.go UnlinkProject goal_projects":                                                                                 "edit: a goal's projects",
	"models/postgres/Guest/guestGrantModel.go ForgetResources guest_grants":                                                                         "purge: the guest grants of resources purged for good",
	"models/postgres/Import/importModel.go SetPriorityMappings import_priority_mappings":                                                            "setting: an import's status and priority mappings, replaced as a set",
	"models/postgres/Import/importModel.go SetStatusMappings import_status_mappings":                                                                "setting: an import's status and priority mappings, replaced as a set",
	"models/postgres/Import/tokenModel.go DeleteToken import_oauth_tokens":                                                                          "secret: an importer's OAuth token",
	"models/postgres/ImportReview/reviews.go DropGuesses import_member_reviews":                                                                     "derived: an import's guessed member matches, dropped once answered",
	"models/postgres/MessageDraft/draftModel.go Delete message_drafts":                                                                              "ephemeral: a person's unsent draft",
	"models/postgres/MessagePin/pinModel.go Forget message_pins":                                                                                    "purge: the pins of messages that are gone",
	"models/postgres/MessagePin/pinModel.go Remove message_pins":                                                                                    "toggle: a message unpinned",
	"models/postgres/Milestone/milestoneModel.go SetTasks task_milestones":                                                                          "edit: a task's milestone; the task keeps the change in its history",
	"models/postgres/Milestone/milestoneModel.go Unshare milestone_projects":                                                                        "edit: a milestone taken off a project it was shared to",
	"models/postgres/Milestone/milestoneModel.go Unshare task_milestones":                                                                           "edit: a task's milestone; the task keeps the change in its history",
	"models/postgres/NotificationEmailLog/notificationEmailLogModel.go CleanupOld notification_email_log":                                           "purge: notification email rows past their retention",
	"models/postgres/NotificationEmailQueue/notificationEmailQueueModel.go CleanupOld notification_email_queue":                                     "purge: notification email rows past their retention",
	"models/postgres/OAuth/oauthModel.go TakeCode oauth_codes":                                                                                      "secret: one-time OAuth requests and codes, used or expired",
	"models/postgres/OAuth/oauthModel.go TakeRequest oauth_requests":                                                                                "secret: one-time OAuth requests and codes, used or expired",
	"models/postgres/OAuth/oauthModel.go prune oauth_codes":                                                                                         "secret: one-time OAuth requests and codes, used or expired",
	"models/postgres/OAuth/oauthModel.go prune oauth_requests":                                                                                      "secret: one-time OAuth requests and codes, used or expired",
	"models/postgres/Outbox/outboxModel.go Purge outbox":                                                                                            "purge: outbox rows already delivered, past their retention",
	"models/postgres/Passkey/passkeyModel.go Delete user_passkeys":                                                                                  "secret: a removed passkey must never sign anyone in again",
	"models/postgres/PersonalAlert/personalAlertModel.go SetKeywords user_notification_keywords":                                                    "setting: a person's alert keywords and VIPs, replaced as a set",
	"models/postgres/PersonalAlert/personalAlertModel.go SetVIPs user_vips":                                                                         "setting: a person's alert keywords and VIPs, replaced as a set",
	"models/postgres/Places/placesModel.go Remove user_places":                                                                                      "toggle: a person's sidebar place",
	"models/postgres/Places/placesModel.go RemoveEverywhere user_places":                                                                            "purge: the sidebar places of resources that are gone",
	"models/postgres/Poll/pollModel.go Delete polls":                                                                                                "TO-SOFT: a poll a person made",
	"models/postgres/Poll/pollModel.go ReplaceVotes poll_votes":                                                                                     "toggle: a person's poll vote, replaced by their new choice",
	"models/postgres/Preference/preferenceModel.go Clear person_preferences":                                                                        "setting: a person's preference back to its default",
	"models/postgres/ProjectPlan/planModel.go RestoreLabel project_plan_labels":                                                                     "edit: a project's labels",
	"models/postgres/ProjectPlan/planModel.go Save project_plan_labels":                                                                             "edit: a project's labels",
	"models/postgres/ProjectRate/projectRateModel.go Clear project_billing":                                                                         "TO-SOFT: a project's billing settings, cleared",
	"models/postgres/ProjectRate/projectRateModel.go Set project_person_rates":                                                                      "TO-SOFT: a person's billing rate on a project, cleared",
	"models/postgres/Proposal/proposalModel.go Store.Delete edit_proposals":                                                                         "TO-SOFT: an edit someone proposed",
	"models/postgres/Proposal/proposalModel.go Store.Sweep edit_proposals":                                                                          "purge: proposals stuck preparing past their window",
	"models/postgres/ResourceView/viewDays.go PruneDays resource_view_days":                                                                         "purge: daily view counts past their retention",
	"models/postgres/SavedItem/savedItemModel.go Delete saved_items":                                                                                "toggle: a person's saved item",
	"models/postgres/SlackBridge/slackBridgeModel.go DeleteBridge slack_bridge":                                                                     "secret: the Slack bridge with its bot token",
	"models/postgres/SlackBridge/slackBridgeModel.go DeleteBridge slack_bridge_links":                                                               "derived: a bridge's channel links, gone with the bridge or its Slack workspace",
	"models/postgres/SlackBridge/slackBridgeModel.go DeleteBridge slack_bridge_messages":                                                            "derived: the Slack message id map",
	"models/postgres/SlackBridge/slackBridgeModel.go DeleteLink slack_bridge_links":                                                                 "TO-SOFT: a Slack channel link an admin made",
	"models/postgres/SlackBridge/slackBridgeModel.go DeleteLink slack_bridge_messages":                                                              "derived: the Slack message id map",
	"models/postgres/SlackBridge/slackBridgeModel.go DeleteMessage slack_bridge_messages":                                                           "derived: the Slack message id map",
	"models/postgres/SlackBridge/slackBridgeModel.go ReleaseClaim slack_bridge_messages":                                                            "derived: the Slack message id map",
	"models/postgres/SlackBridge/slackBridgeModel.go SaveBridge slack_bridge_links":                                                                 "derived: a bridge's channel links, gone with the bridge or its Slack workspace",
	"models/postgres/SlackBridge/slackBridgeModel.go SaveBridge slack_bridge_messages":                                                              "derived: the Slack message id map",
	"models/postgres/SlackImport/slackImportModel.go ForgetMapped import_id_map":                                                                    "derived: an import's id map entry, so a re-import brings the record again",
	"models/postgres/SlackImport/slackImportModel.go ForgetMapped import_workspace_id_map":                                                          "derived: an import's id map entry, so a re-import brings the record again",
	"models/postgres/TaskChange/taskChangeModel.go DeleteBefore task_changes":                                                                       "purge: task change history past its retention",
	"models/postgres/TaskField/taskFieldModel.go SetValue task_field_values":                                                                        "edit: a task's value of a field; the task keeps the change in its history",
	"models/postgres/TaskField/taskFieldModel.go dropOptionValues task_field_values x2":                                                             "edit: a task's value of a field; the task keeps the change in its history",
	"models/postgres/TaskKey/taskKeyModel.go SetKey project_key_aliases":                                                                            "derived: task key aliases, moved when a project takes a key",
	"models/postgres/TaskKey/taskKeyModel.go TakeKey project_key_aliases x2":                                                                        "derived: task key aliases, moved when a project takes a key",
	"models/postgres/TaskKey/taskKeyModel.go TakeKey project_keys":                                                                                  "derived: task key aliases, moved when a project takes a key",
	"models/postgres/TaskRecurrence/taskRecurrenceModel.go Delete task_recurrences":                                                                 "edit: a task's repeat rule",
	"models/postgres/TaskRecurrence/taskRecurrenceModel.go Take task_recurrences":                                                                   "derived: a repeat rule handed on to the task's next instance",
	"models/postgres/TaskView/taskViewModel.go Delete task_views":                                                                                   "TO-SOFT: a saved view a person made",
	"models/postgres/Thread/comments.go RemoveReaction thread_comment_reactions":                                                                    "toggle: a person's reaction",
	"models/postgres/Thread/threadModel.go Delete threads":                                                                                          "TO-SOFT: a thread a person started",
	"models/postgres/Thread/threadModel.go ForgetResources thread_comments":                                                                         "purge: the threads, with their comments, of resources purged for good",
	"models/postgres/Thread/threadModel.go ForgetResources threads":                                                                                 "purge: the threads, with their comments, of resources purged for good",
	"models/postgres/TimeEntry/timeEntryModel.go ClearForPersonInProject task_time_entries":                                                         "demo: the demo reset clears its visitor's own time entries",
	"models/postgres/User/mergeBackups.go SettleMergeBatches person_merge_backups":                                                                  "rollback: backup rows of a merge whose graph change never landed",
	"models/postgres/User/mergeBackups.go dropMergeBatch person_merge_backups":                                                                      "rollback: an earlier try's pending backup rows of the same merge batch",
	"models/postgres/User/totpModel.go ConfirmTOTPEnrollment user_recovery_codes":                                                                   "secret: recovery codes replaced by a new set or revoked with two-step sign-in",
	"models/postgres/User/totpModel.go DisableTOTP user_recovery_codes":                                                                             "secret: recovery codes replaced by a new set or revoked with two-step sign-in",
	"models/postgres/WebhookDelivery/webhookDeliveryModel.go Purge webhook_deliveries":                                                              "purge: webhook deliveries past their retention",
	"models/postgres/WhoKnows/optOutModel.go SetListed who_knows_opt_outs":                                                                          "toggle: a person's opt-out from who-knows answers",
	"models/postgres/Workflow/ruleModel.go PurgeRuns workflow_runs":                                                                                 "purge: workflow runs past their retention",
	"models/postgres/WorkspaceMemory/exclusionModel.go SetScopeExcluded ai_memory_exclusions":                                                       "TO-SOFT: a channel's AI memory exclusion, a workspace-trust control its admins set: keep who turned memory off and back on",
	"models/postgres/WorkspaceNudge/workspaceNudgeModel.go PurgeOldTerminal workspace_nudges":                                                       "purge: finished nudges past their retention",
}

// deleteFromRe finds a DELETE FROM and the table it names ("" when the
// statement builds it, which the key calls "?").
var deleteFromRe = regexp.MustCompile(`(?i)\bDELETE\s+FROM\s+(?:ONLY\s+)?"?([A-Za-z_%][A-Za-z0-9_.]*)?`)

// findHardDeletes keys every DELETE FROM in the string literals of the
// repository's non-test Go, counted per key, and how many files it read.
func findHardDeletes(t *testing.T) (map[string]int, int) {
	t.Helper()
	found := map[string]int{}
	files := 0
	fset := token.NewFileSet()
	err := filepath.Walk("..", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case "node_modules", ".git", "data", "vendor", ".next":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files++
		rel := filepath.ToSlash(strings.TrimPrefix(path, ".."+string(filepath.Separator)))
		for _, d := range f.Decls {
			name := hardDeleteDecl(d)
			ast.Inspect(d, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					s = lit.Value
				}
				for _, m := range deleteFromRe.FindAllStringSubmatch(s, -1) {
					table := m[1]
					if table == "" {
						table = "?"
					}
					found[rel+" "+name+" "+table]++
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	keyed := map[string]int{}
	for k, n := range found {
		if n > 1 {
			k += " x" + strconv.Itoa(n)
		}
		keyed[k] = n
	}
	return keyed, files
}

// hardDeleteDecl is what a key calls a top-level declaration: a function's name,
// a method's as Type.Name, a var or const by its first name.
func hardDeleteDecl(d ast.Decl) string {
	switch d := d.(type) {
	case *ast.FuncDecl:
		if d.Recv != nil && len(d.Recv.List) > 0 {
			typ := d.Recv.List[0].Type
			if star, ok := typ.(*ast.StarExpr); ok {
				typ = star.X
			}
			if id, ok := typ.(*ast.Ident); ok {
				return id.Name + "." + d.Name.Name
			}
		}
		return d.Name.Name
	case *ast.GenDecl:
		for _, s := range d.Specs {
			if v, ok := s.(*ast.ValueSpec); ok && len(v.Names) > 0 {
				return v.Names[0].Name
			}
		}
	}
	return "_"
}

func TestEveryHardDeleteIsReasoned(t *testing.T) {
	found, files := findHardDeletes(t)
	if files < 1000 || len(found) == 0 {
		t.Fatalf("read %d files and found %d deletes; the guard is no longer looking at the repository", files, len(found))
	}
	var unlisted, stale []string
	for k := range found {
		if _, ok := hardDeletes[k]; !ok {
			unlisted = append(unlisted, k)
		}
	}
	for k := range hardDeletes {
		if _, ok := found[k]; !ok {
			stale = append(stale, k)
		}
	}
	sort.Strings(unlisted)
	sort.Strings(stale)
	if len(unlisted) > 0 {
		t.Errorf("%d hard deletes are new: a business record's delete is soft (models/postgres/SoftDelete); "+
			"anything else goes in hardDeletes with its reason:\n  %s", len(unlisted), strings.Join(unlisted, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("%d hardDeletes entries match no DELETE FROM any more: remove them (a TO-SOFT one lowers toSoftBaseline):\n  %s",
			len(stale), strings.Join(stale, "\n  "))
	}
}

func TestHardDeleteReasonsNameAClass(t *testing.T) {
	toSoft := 0
	for k, why := range hardDeletes {
		class := ""
		for _, c := range hardDeleteClasses {
			if strings.HasPrefix(why, c) {
				class = c
			}
		}
		if class == "" || len(strings.TrimSpace(strings.TrimPrefix(why, class))) < 12 {
			t.Errorf("%s: %q starts with one of hardDeleteClasses and says why in words", k, why)
		}
		if class == "TO-SOFT: " {
			toSoft++
		}
	}
	switch {
	case toSoft > toSoftBaseline:
		t.Errorf("%d TO-SOFT entries, more than %d: a new business record's delete is soft from the start", toSoft, toSoftBaseline)
	case toSoft < toSoftBaseline:
		t.Errorf("%d TO-SOFT entries now, fewer than %d: lower toSoftBaseline to %d", toSoft, toSoftBaseline, toSoft)
	}
}
