package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	opensearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
)

const CHAT_AND_COMMENT_COUNT = 10
const POST_AND_COMMENT_COUNT = 10
const ATTACHMENT_COUNT = 10
const UNIFIED_SEARCH_COUNT = 20

func GetUnifiedGlobalSearchFromOpenSearch(ctx context.Context, userUUID string, userEmail string, channelUUIDs []string, projectUUIDs []string, teamUUIDs []string, searchText string) (searchInfo []*openSearchStruct.GlobalSearchOpenSearchResp, err error) {

	if channelUUIDs == nil {
		channelUUIDs = []string{}
	}
	channelUUIDByte, err := json.Marshal(channelUUIDs)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetUnifiedGlobalSearchFromOpenSearch Failed to marshal channelUUIDs json array err: %+v",
			err,
		)
		return
	}
	channelUUIDString := string(channelUUIDByte)

	if projectUUIDs == nil {
		projectUUIDs = []string{}
	}
	projectUUIDByte, err := json.Marshal(projectUUIDs)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetUnifiedGlobalSearchFromOpenSearch Failed to marshal projectUUIDs json array err: %+v",
			err,
		)
		return
	}
	projectUUIDString := string(projectUUIDByte)

	if teamUUIDs == nil {
		teamUUIDs = []string{}
	}
	teamUUIDByte, err := json.Marshal(teamUUIDs)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetUnifiedGlobalSearchFromOpenSearch Failed to marshal teamUUIDs json array err: %+v",
			err,
		)
		return
	}
	teamUUIDString := string(teamUUIDByte)

	searchTextByte, _ := json.Marshal(searchText)
	searchTextEscaped := string(searchTextByte)

	userUUIDByte, _ := json.Marshal(userUUID)
	userUUIDEscaped := string(userUUIDByte)

	// People are found by their address as well as their name, except by the
	// demo's shared visitor, who is anyone at all: fuzziness finds an address
	// within two edits of a guess, so a stranger could learn whose address a
	// guess is, or confirm one (helpers.ServeHidingEmails). And no one's address
	// is highlighted: a person's hit carries it anyway, and highlighted
	// (<mark>someone@example.com</mark>) it got past the demo's hiding of
	// addresses.
	userFields := `["user_name^5", "user_full_name^5", "user_email", "user_id"]`
	if helpers.IsDemoVisitor(userEmail) {
		userFields = `["user_name^5", "user_full_name^5", "user_id"]`
	}

	// doc_body is excluded from _source because it is a field to SEARCH, not one to
	// return: a doc result renders doc_title, the highlight fragments, and
	// doc_snippet, and nothing downstream reads the body out of a hit. Returning it
	// meant every doc match shipped the entire document — so a handful of hits on
	// long documents allocated all of that on the search node's heap and then again
	// in this process, for data no caller used. That is the same exposure that
	// OOM-killed the node on the write side, reached from the read side instead.
	// Highlighting still works: it reads the indexed field, not _source.
	query := fmt.Sprintf(`{
	"size": %v,
	"_source": { "excludes": ["doc_body"] },
	"highlight": {
		"pre_tags": ["<mark>"],
		"post_tags": ["</mark>"],
		"fields": {
			"chat_body": {},
			"post_body": {},
			"comment_body": {},
			"attachment_file_name": {},
			"doc_title": {},
			"doc_body": {},
			"task_name": {},
			"task_desc": {},
			"project_name": {},
			"team_name": {},
			"ch_name": {},
			"user_name": {}
		}
	},
	"query": {
		"bool": {
			"should": [
				{
					"bool": {
						"must": [
							{ "term": { "_index": "chats" } },
							{
								"bool": {
									"should": [
										{ "term": { "chat_to_user_id": %s } },
										{ "term": { "chat_by_user_id": %s } },
										{ "nested": { "path": "chat_participants", "ignore_unmapped": true, "query": { "term": { "chat_participants.user_uuid": %s } } } }
									]
								}
							},
							{ 
								"multi_match": {
									"query": %s,
									"fields": ["chat_body", "chat_by_user_full_name", "chat_participants.user_name"]
								}
							}
						],
						"filter": { "bool": { "must_not": [ { "exists": { "field": "deleted_date" } } ] } }
					}
				},
				{
					"bool": {
						"must": [
							{ "term": { "_index": "posts" } },
							{ "terms": { "post_ch_id": %s } },
							{ 
								"multi_match": {
									"query": %s,
									"fields": ["post_body", "post_by_user_full_name"]
								}
							}
						],
						"filter": { "bool": { "must_not": [ { "exists": { "field": "deleted_date" } } ] } }
					}
				},
				{
					"bool": {
						"must": [
							{ "term": { "_index": "comments" } },
							{
								"bool": {
									"should": [
										{ "terms": { "comment_channel_id": %s } },
										{ "term": { "comment_chat_to_user_id": %s } },
										{ "term": { "comment_chat_from_user_id": %s } },
										{ "nested": { "path": "comment_chat_participants", "ignore_unmapped": true, "query": { "term": { "comment_chat_participants.user_uuid": %s } } } },
										{ "terms": { "comment_project_id": %s } },
										{ "term": { "comment_doc_created_by_user_id": %s } },
										{ "term": { "comment_doc_reading_users": %s } },
										{ "term": { "comment_doc_editing_users": %s } },
										{ "term": { "comment_doc_commenting_users": %s } },
										{ "term": { "comment_doc_private": false } },
										{ "term": { "comment_board_created_by_user_id": %s } },
										{ "term": { "comment_board_reading_users": %s } },
										{ "term": { "comment_board_editing_users": %s } },
										{ "term": { "comment_board_commenting_users": %s } },
										{ "term": { "comment_board_public": true } }
									],
									"minimum_should_match": 1
								}
							},
							{ 
								"bool": {
									"should": [
										{ "multi_match": { "query": %s, "fields": ["comment_body^3", "comment_by_user_full_name^2", "comment_chat_from_user_full_name^2", "comment_chat_participants.user_name^2", "comment_doc_title^1.5", "comment_project_name^1.5", "comment_channel_name^1.5"] } },
										{ "match_phrase_prefix": { "comment_body": { "query": %s } } }
									]
								}
							}
						],
						"filter": { "bool": { "must_not": [ { "exists": { "field": "deleted_date" } } ] } }
					}
				},
				{
					"bool": {
						"must": [
							{ "term": { "_index": "attachments" } },
							{
								"bool": {
									"should": [
										{ "terms": { "attachment_channel_id": %s } },
										{ "term": { "attachment_chat_to_user_id": %s } },
										{ "term": { "attachment_chat_from_user_id": %s } },
										{ "nested": { "path": "attachment_chat_participants", "ignore_unmapped": true, "query": { "term": { "attachment_chat_participants.user_uuid": %s } } } },
										{ "terms": { "attachment_project_id": %s } },
										{ "term": { "attachment_doc_created_by_user_id": %s } },
										{ "term": { "attachment_doc_reading_users": %s } },
										{ "term": { "attachment_doc_editing_users": %s } },
										{ "term": { "attachment_doc_commenting_users": %s } },
										{ "term": { "attachment_doc_private": false } }
									],
									"minimum_should_match": 1
								}
							},
							{ 
								"bool": {
									"should": [
										{ "multi_match": { "query": %s, "fields": ["attachment_file_name^3", "attachment_by_user_full_name", "attachment_chat_from_user_name", "attachment_chat_participants.user_name", "attachment_doc_title^1.5", "attachment_project_name^1.5", "attachment_channel_name^1.5"] } },
										{ "match_phrase_prefix": { "attachment_file_name": { "query": %s } } }
									]
								}
							}
						],
						"filter": { "bool": { "must_not": [ { "exists": { "field": "deleted_date" } } ] } }
					}
				},
				{
					"bool": {
						"must": [
							{ "term": { "_index": "docs" } },
							{
								"bool": {
									"should": [
										{ "term": { "doc_created_by_user_id": %s } },
										{ "term": { "doc_reading_users": %s } },
										{ "term": { "doc_editing_users": %s } },
										{ "term": { "doc_commenting_users": %s } },
										{ "term": { "doc_private": false } }
									],
									"minimum_should_match": 1
								}
							},
							{ 
								"bool": {
									"should": [
										{ "multi_match": { "query": %s, "fields": ["doc_title^5", "doc_body", "doc_created_by_user_full_name^2"] } },
										{ "match_phrase_prefix": { "doc_title": { "query": %s, "boost": 10.0 } } }
									]
								}
							}
						],
						"filter": { "bool": { "must_not": [ { "exists": { "field": "deleted_date" } } ] } }
					}
				},
				{
					"bool": {
						"must": [
							{ "term": { "_index": "boards" } },
							{
								"bool": {
									"should": [
										{ "term": { "board_created_by_user_id": %s } },
										{ "term": { "board_reading_users": %s } },
										{ "term": { "board_editing_users": %s } },
										{ "term": { "board_commenting_users": %s } },
										{ "term": { "board_private": false } }
									],
									"minimum_should_match": 1
								}
							},
							{ 
								"bool": {
									"should": [
										{ "multi_match": { "query": %s, "fields": ["board_title^5", "board_snippet", "board_created_by_user_full_name^2"] } },
										{ "match_phrase_prefix": { "board_title": { "query": %s, "boost": 10.0 } } }
									]
								}
							}
						],
						"filter": { "bool": { "must_not": [ { "exists": { "field": "deleted_date" } } ] } }
					}
				},
				{
					"bool": {
						"must": [
							{ "term": { "_index": "tasks" } },
							{
								"bool": {
									"should": [
										{ "terms": { "task_project_id": %s } },
										{ "term": { "task_assignee_user_id": %s } },
										{ "term": { "task_created_by_user_id": %s } }
									],
									"minimum_should_match": 1
								}
							},
							{ 
								"bool": {
									"should": [
										{ "multi_match": { "query": %s, "fields": ["task_name^5", "task_desc", "task_assignee_user_full_name^2", "task_label^3"] } },
										{ "match_phrase_prefix": { "task_name": { "query": %s, "boost": 10.0 } } }
									],
									"minimum_should_match": 1
								}
							}
						],
						"filter": { "bool": { "must_not": [ { "exists": { "field": "deleted_date" } } ] } }
					}
				},
				{
					"bool": {
						"boost": 50.0,
						"must": [
							{ "term": { "_index": "projects" } },
							{ 
								"bool": {
									"should": [
										{ "terms": { "project_id": %s } },
										{ "terms": { "project_team_id": %s } }
									],
									"minimum_should_match": 1
								}
							},
							{ 
								"bool": {
									"should": [
										{ "multi_match": { "query": %s, "fields": ["project_name^5"], "fuzziness": "AUTO" } },
										{ "match_phrase_prefix": { "project_name": { "query": %s, "boost": 10.0 } } }
									],
									"minimum_should_match": 1
								}
							}
						],
						"filter": { "bool": { "must_not": [ { "exists": { "field": "deleted_date" } } ] } }
					}
				},
				{
					"bool": {
						"must": [
							{ "term": { "_index": "channels" } },
							{ "terms": { "ch_uuid": %s } },
							{ 
								"bool": {
									"should": [
										{ "multi_match": { "query": %s, "fields": ["ch_name^5"], "fuzziness": "AUTO" } },
										{ "match_phrase_prefix": { "ch_name": { "query": %s, "boost": 10.0 } } }
									],
									"minimum_should_match": 1
								}
							}
						],
						"filter": { "bool": { "must_not": [ { "exists": { "field": "deleted_date" } } ] } }
					}
				},
				{
					"bool": {
						"must": [
							{ "term": { "_index": "teams" } },
							{ "terms": { "team_id": %s } },
							{ 
								"bool": {
									"should": [
										{ "multi_match": { "query": %s, "fields": ["team_name^5"], "fuzziness": "AUTO" } },
										{ "match_phrase_prefix": { "team_name": { "query": %s, "boost": 10.0 } } }
									],
									"minimum_should_match": 1
								}
							}
						],
						"filter": { "bool": { "must_not": [ { "exists": { "field": "deleted_date" } } ] } }
					}
				},
				{
					"bool": {
						"boost": 15.0,
						"must": [
							{ "term": { "_index": "users" } },
							{
								"bool": {
									"should": [
										{ 
											"multi_match": {
												"query": %s,
												"fields": %s,
												"fuzziness": "AUTO"
											}
										},
										{
											"match_phrase_prefix": {
												"user_name": {
													"query": %s,
													"boost": 2.0
												}
											}
										},
										{
											"match_phrase_prefix": {
												"user_full_name": {
													"query": %s,
													"boost": 2.0
												}
											}
										}
									],
									"minimum_should_match": 1
								}
							}
						],
						"filter": { "bool": { "must_not": [ { "exists": { "field": "deleted_date" } } ] } }
					}
				}
			]
		}
	},
	"sort": [
		{ "_score": { "order": "desc" } },
		{ "created_date": { "order": "desc" } }
	]
	}`, UNIFIED_SEARCH_COUNT+1,
		userUUIDEscaped, userUUIDEscaped, userUUIDEscaped, searchTextEscaped,
		channelUUIDString, searchTextEscaped,
		channelUUIDString, userUUIDEscaped, userUUIDEscaped, userUUIDEscaped, projectUUIDString, userUUIDEscaped, userUUIDEscaped, userUUIDEscaped, userUUIDEscaped, userUUIDEscaped, userUUIDEscaped, userUUIDEscaped, userUUIDEscaped, searchTextEscaped, searchTextEscaped,
		channelUUIDString, userUUIDEscaped, userUUIDEscaped, userUUIDEscaped, projectUUIDString, userUUIDEscaped, userUUIDEscaped, userUUIDEscaped, userUUIDEscaped, searchTextEscaped, searchTextEscaped,
		userUUIDEscaped, userUUIDEscaped, userUUIDEscaped, userUUIDEscaped, searchTextEscaped, searchTextEscaped,
		userUUIDEscaped, userUUIDEscaped, userUUIDEscaped, userUUIDEscaped, searchTextEscaped, searchTextEscaped,
		projectUUIDString, userUUIDEscaped, userUUIDEscaped, searchTextEscaped, searchTextEscaped,
		projectUUIDString, teamUUIDString, searchTextEscaped, searchTextEscaped,
		channelUUIDString, searchTextEscaped, searchTextEscaped,
		teamUUIDString, searchTextEscaped, searchTextEscaped,
		searchTextEscaped, userFields, searchTextEscaped, searchTextEscaped)

	searchInfo, err = opensearchModels.GetUnifiedGlobalSearchFromOpenSearch(ctx, query)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetUnifiedGlobalSearchFromOpenSearch Failed to get unified search from opensearch err: %+v",
			err,
		)
		return
	}

	return
}

func GetLatestChatsAndCommentsFromOpenSearch(ctx context.Context, userUUID string, searchText string) (chatsAndCommentsInfo []*openSearchStruct.GlobalSearchOpenSearchResp, err error) {

	searchTextByte, _ := json.Marshal(searchText)
	searchTextEscaped := string(searchTextByte)

	userUUIDByte, _ := json.Marshal(userUUID)
	userUUIDEscaped := string(userUUIDByte)

	query := fmt.Sprintf(`{
	"size": %v,
	"query": {
		"bool": {
			"should": [
				{
					"bool": {
						"must": [
							{
								"bool": {
									"should": [
										{
											"term": {
												"comment_chat_to_user_id": %s
											}
										},
										{
											"term": {
												"comment_chat_from_user_id": %s
											}
										}
									]
								}
							},
							{
								"match": {
									"comment_body": %s
								}
							}
						],
						"filter": {
							"bool": {
								"must_not": [
									{
										"exists": {
											"field": "deleted_date"
										}
									}
								]
							}
						}
					}
				},
				{
					"bool": {
						"must": [
							{
								"bool": {
									"should": [
										{
											"term": {
												"chat_to_user_id": %s
											}
										},
										{
											"term": {
												"chat_by_user_id": %s
											}
										}
									]
								}
							},
							{
								"match": {
									"chat_body": %s
								}
							}
						],
						"filter": {
							"bool": {
								"must_not": [
									{
										"exists": {
											"field": "deleted_date"
										}
									}
								]
							}
						}
					}
				}
			]
		}
	},
	"sort": [
		{
			"created_date": {
				"order": "desc"
			}
		}
	]
	}`, CHAT_AND_COMMENT_COUNT+1, userUUIDEscaped, userUUIDEscaped, searchTextEscaped, userUUIDEscaped, userUUIDEscaped, searchTextEscaped)

	chatsAndCommentsInfo, err = opensearchModels.GetChatsAndCommentsFromOpenSearch(ctx, query)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetLatestCommentAndChatGlobalSearchInOpenSearch Failed to get chat and comment from opensearch err: %+v",
			err,
		)
		return
	}

	return
}

func GetLatestChatsAndCommentsFromOpenSearchBeforeTime(ctx context.Context, userUUID string, afterTime time.Time, searchText string) (chatsAndCommentsInfo []*openSearchStruct.GlobalSearchOpenSearchResp, err error) {

	afterTimeUnix := afterTime.Unix()
	searchTextByte, _ := json.Marshal(searchText)
	searchTextEscaped := string(searchTextByte)

	userUUIDByte, _ := json.Marshal(userUUID)
	userUUIDEscaped := string(userUUIDByte)

	query := fmt.Sprintf(`{
	"size": %v,
	"query": {
		"bool": {
			"should": [
				{
					"bool": {
						"must": [
							{
								"bool": {
									"should": [
										{
											"term": {
												"comment_chat_to_user_id": %s
											}
										},
										{
											"term": {
												"comment_chat_from_user_id": %s
											}
										}
									]
								}
							},
							{
								"match": {
									"comment_body": %s
								}
							}
						],
						"filter": {
							"bool": {
								"must_not": [
									{
										"exists": {
											"field": "deleted_date"
										}
									},
									{
										"range": {
										  "created_date": {
											"gte": %v
										  }
										}
									}
								]
							}
						}
					}
				},
				{
					"bool": {
						"must": [
							{
								"bool": {
									"should": [
										{
											"term": {
												"chat_to_user_id": %s
											}
										},
										{
											"term": {
												"chat_by_user_id": %s
											}
										}
									]
								}
							},
							{
								"match": {
									"chat_body": %s
								}
							}
						],
						"filter": {
							"bool": {
								"must_not": [
									{
										"exists": {
											"field": "deleted_date"
										}
									},
									{
										"range": {
										  "created_date": {
											"gte": %v
										  }
										}
									}
								]
							}
						}
					}
				}
			]
		}
	},
	"sort": [
		{
			"created_date": {
				"order": "desc"
			}
		}
	]
	}`, CHAT_AND_COMMENT_COUNT+1, userUUIDEscaped, userUUIDEscaped, searchTextEscaped, afterTimeUnix, userUUIDEscaped, userUUIDEscaped, searchTextEscaped, afterTimeUnix)

	chatsAndCommentsInfo, err = opensearchModels.GetChatsAndCommentsFromOpenSearch(ctx, query)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetLatestChatsAndCommentsFromOpenSearchBeforeTime Failed to get chat and comment from opensearch err: %+v",
			err,
		)
		return
	}

	return
}

func GetLatestPostsAndCommentsFromOpenSearch(ctx context.Context, channelUUIDs []string, searchText string) (postsAndCommentsInfo []*openSearchStruct.GlobalSearchOpenSearchResp, err error) {

	channelUUIDByte, err := json.Marshal(channelUUIDs)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetLatestPostsAndCommentsFromOpenSearch Failed to marshal json array err: %+v",
			err,
		)
		return
	}

	channelUUIDString := string(channelUUIDByte)

	searchTextByte, _ := json.Marshal(searchText)
	searchTextEscaped := string(searchTextByte)

	query := fmt.Sprintf(`{
	"size": %v,
	"query": {
		"bool": {
			"should": [
				{
					"bool": {
						"must": [
							{
								"terms": {
									"post_ch_id": %s
								}
        					},
							{
								"match": {
									"post_body": %s
								}
							}
						],
						"filter": {
							"bool": {
								"must_not": [
									{
										"exists": {
											"field": "deleted_date"
										}
									}
								]
							}
						}
					}
				},
				{
					"bool": {
						"must": [
							{
								"terms": {
									"comment_channel_id": %s
								}
        					},
							{
								"match": {
									"comment_body": %s
								}
							}
						],
						"filter": {
							"bool": {
								"must_not": [
									{
										"exists": {
											"field": "deleted_date"
										}
									}
								]
							}
						}
					}
				}
			]
		}
	},
	"sort": [
		{
			"created_date": {
				"order": "desc"
			}
		}
	]
	}`, POST_AND_COMMENT_COUNT+1, channelUUIDString, searchTextEscaped, channelUUIDString, searchTextEscaped)

	postsAndCommentsInfo, err = opensearchModels.GetPostsAndCommentsFromOpenSearch(ctx, query)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetLatestPostsAndCommentsFromOpenSearch Failed to get post and comment from opensearch err: %+v",
			err,
		)
		return
	}

	return
}

func GetLatestPostsAndCommentsFromOpenSearchBeforeTime(ctx context.Context, channelUUIDs []string, afterTIme time.Time, searchText string) (postsAndCommentsInfo []*openSearchStruct.GlobalSearchOpenSearchResp, err error) {

	afterTimeUnix := afterTIme.Unix()
	channelUUIDByte, err := json.Marshal(channelUUIDs)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetLatestPostsAndCommentsFromOpenSearch Failed to marshal json array err: %+v",
			err,
		)
		return
	}

	channelUUIDString := string(channelUUIDByte)

	searchTextByte, _ := json.Marshal(searchText)
	searchTextEscaped := string(searchTextByte)

	query := fmt.Sprintf(`{
	"size": %v,
	"query": {
		"bool": {
			"should": [
				{
					"bool": {
						"must": [
							{
								"terms": {
									"post_ch_id": %s
								}
        					},
							{
								"match": {
									"post_body": %s
								}
							}
						],
						"filter": {
							"bool": {
								"must_not": [
									{
										"exists": {
											"field": "deleted_date"
										}
									},
									{
										"range": {
										  "created_date": {
											"gte": %v
										  }
										}
									}
								]
							}
						}
					}
				},
				{
					"bool": {
						"must": [
							{
								"terms": {
									"comment_channel_id": %s
								}
        					},
							{
								"match": {
									"comment_body": %s
								}
							}
						],
						"filter": {
							"bool": {
								"must_not": [
									{
										"exists": {
											"field": "deleted_date"
										}
									},
									{
										"range": {
										  "created_date": {
											"gte": %v
										  }
										}
									}
								]
							}
						}
					}
				}
			]
		}
	},
	"sort": [
		{
			"created_date": {
				"order": "desc"
			}
		}
	]
	}`, POST_AND_COMMENT_COUNT+1, channelUUIDString, searchTextEscaped, afterTimeUnix, channelUUIDString, searchTextEscaped, afterTimeUnix)

	postsAndCommentsInfo, err = opensearchModels.GetPostsAndCommentsFromOpenSearch(ctx, query)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetLatestPostsAndCommentsFromOpenSearch Failed to get post and comment from opensearch err: %+v",
			err,
		)
		return
	}

	return
}

func GetLatestAttachmentsFromOpenSearchBeforeTime(ctx context.Context, userUUID string, channelUUIDs []string, afterTIme time.Time, searchText string) (attachmentsInfo []*openSearchStruct.GlobalSearchOpenSearchResp, err error) {

	afterTimeUnix := afterTIme.Unix()
	channelUUIDByte, err := json.Marshal(channelUUIDs)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetLatestAttachmentsFromOpenSearchBeforeTime Failed to marshal json array err: %+v",
			err,
		)
		return
	}

	channelUUIDString := string(channelUUIDByte)

	userUUIDByte, _ := json.Marshal(userUUID)
	userUUIDEscaped := string(userUUIDByte)

	query := fmt.Sprintf(`{
	"size": %v,
	"query": {
		"bool": {
			"must": [
				{
					"bool": {
						"should": [
							{
								"terms": {
									"attachment_channel_id": %s
								}
        					},
							{
								"term": {
									"attachment_chat_to_user_id": %s
								}
							},
							{
								"term": {
									"attachment_chat_from_user_id": %s
								}
							}
						]
					}
				},
				{
						
					"wildcard": {
                        "attachment_file_name": "*%s*"
                    }
						
				}
			],
			"filter": {
				"bool": {
					"must_not": [
						{
							"exists": {
								"field": "deleted_date"
							}
						},
						{
							"range": {
							  "created_date": {
								"gte": %v
							  }
							}
						}
					]
				}
			}
		}
	},
	"sort": [
		{
			"created_date": {
				"order": "desc"
			}
		}
	]
	}`, ATTACHMENT_COUNT+1, channelUUIDString, userUUIDEscaped, userUUIDEscaped, searchText, afterTimeUnix)

	attachmentsInfo, err = opensearchModels.GetAttachmentsFromOpenSearch(ctx, query)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetLatestAttachmentsFromOpenSearchBeforeTime Failed to get attachment from opensearch err: %+v",
			err,
		)
		return
	}

	return
}

func GetLatestAttachmentsFromOpenSearch(ctx context.Context, userUUID string, channelUUIDs []string, searchText string) (attachmentsInfo []*openSearchStruct.GlobalSearchOpenSearchResp, err error) {

	channelUUIDByte, err := json.Marshal(channelUUIDs)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetLatestAttachmentsFromOpenSearch Failed to marshal json array err: %+v",
			err,
		)
		return
	}

	channelUUIDString := string(channelUUIDByte)

	userUUIDByte, _ := json.Marshal(userUUID)
	userUUIDEscaped := string(userUUIDByte)

	query := fmt.Sprintf(`{
    "size": %d,
    "query": {
        "bool": {
            "must": [
                {
                    "bool": {
                        "should": [
                            {
                                "terms": {
                                    "attachment_channel_id": %s
                                }
                            },
                            {
                                "term": {
                                    "attachment_chat_to_user_id": %s
                                }
                            },
                            {
                                "term": {
                                    "attachment_chat_from_user_id": %s
                                }
                            }
                        ]
                    }
                },
                {
                    "wildcard": {
                        "attachment_file_name": "*%s*"
                    }
                }
            ],
            "filter": {
                "bool": {
                    "must_not": [
                        {
                            "exists": {
                                "field": "deleted_date"
                            }
                        }
                    ]
                }
            }
        }
    },
    "sort": [
        {
            "created_date": {
                "order": "desc"
            }
        }
    ]
	}`, ATTACHMENT_COUNT+1, channelUUIDString, userUUIDEscaped, userUUIDEscaped, searchText)

	attachmentsInfo, err = opensearchModels.GetAttachmentsFromOpenSearch(ctx, query)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetLatestAttachmentsFromOpenSearch Failed to get attachment from opensearch err: %+v",
			err,
		)
		return
	}

	return
}

func SyncCascadingDeletionInOpenSearch(parentFields []string, parentID string, deletedAt int64, indices []string, deletedBy string) (err error) {
	err = opensearchModels.SyncCascadingDeletionInOpenSearch(context.Background(), parentFields, parentID, deletedAt, indices, deletedBy)
	if err != nil {
		helpers.LogErrorWithContext(context.Background(),
			"domain/SyncCascadingDeletionInOpenSearch Failed to cascade deletion err: %+v",
			err)
		return
	}

	return
}

func SyncCascadingUnarchiveInOpenSearch(parentFields []string, parentID string, indices []string, deletedBy string) (err error) {
	err = opensearchModels.SyncCascadingUnarchiveInOpenSearch(context.Background(), parentFields, parentID, indices, deletedBy)
	if err != nil {
		helpers.LogErrorWithContext(context.Background(),
			"domain/SyncCascadingUnarchiveInOpenSearch Failed to cascade unarchive err: %+v",
			err)
		return
	}

	return
}

func SyncCascadingDeletionInOpenSearchMulti(parentField string, parentIDs []string, deletedAt int64, indices []string, deletedBy string) {
	opensearchModels.SyncCascadingDeletionInOpenSearchMulti(context.Background(), parentField, parentIDs, deletedAt, indices, deletedBy)
}

func SyncCascadingUnarchiveInOpenSearchMulti(parentField string, parentIDs []string, indices []string, deletedBy string) {
	opensearchModels.SyncCascadingUnarchiveInOpenSearchMulti(context.Background(), parentField, parentIDs, indices, deletedBy)
}

func SyncCascadingDeletionInOpenSearchCombined(fieldIDs map[string][]string, deletedAt int64, indices []string, deletedBy string) {
	opensearchModels.SyncCascadingDeletionInOpenSearchCombined(context.Background(), fieldIDs, deletedAt, indices, deletedBy)
}

func SyncCascadingUnarchiveInOpenSearchCombined(fieldIDs map[string][]string, indices []string, deletedBy string) {
	opensearchModels.SyncCascadingUnarchiveInOpenSearchCombined(context.Background(), fieldIDs, indices, deletedBy)
}
