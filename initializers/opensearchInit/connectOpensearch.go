package opensearchInit

import (
	"context"
	"crypto/tls"
	"net/http"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

var OpenSearchClient *opensearchapi.Client

type OpenSearchConfig struct {
	Host     string
	Username string
	Password string
}

// The indices this install needs, populated when ConnectOpenSearch runs.
//
// Package level so the search health check asks about exactly the indices this
// code creates. A probe with its own copy of the list would keep passing after
// an index was added here, which is the failure it exists to catch.
//
// nil therefore means something real: ConnectOpenSearch never got far enough to
// declare them, so search was never initialised at all.
var indicesToCreate map[string]string

func ConnectOpenSearch(config *OpenSearchConfig) (err error) {
	ctx := context.Background()
	OpenSearchClient, err = opensearchapi.NewClient(
		opensearchapi.Config{
			Client: opensearch.Config{
				Transport: &http.Transport{
					TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // For testing only. Use certificate for validation.
				},
				Addresses: []string{config.Host},
				Username:  config.Username,
				Password:  config.Password,
			},
		},
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "opensearchInit/connectOpenSearch Failed to connect to openSearch err:= %+v", err)
		return err
	}

	helpers.MessageLogs.InfoLog.Println("Successfully connected with openSearch server !")

	indicesToCreate = map[string]string{
		"posts": `{
			"settings": { "index": { "number_of_shards": 1, "number_of_replicas": 0 } },
			"mappings": {
				"properties": {
					"post_body": { "type": "text", "index": true },
					"post_by_user_id": { "type": "keyword", "index": false },
					"post_by_user_full_name": { "type": "text", "index": true },
					"post_by_profile": { "type": "keyword", "index": false },
					"post_id": { "type": "keyword", "index": false },
					"post_ch_id": { "type": "keyword", "index": true},
					"post_ch_name": { "type": "text", "index": true},
					"created_date": { "type": "date", "format": "epoch_second", "index": true },
					"updated_date": { "type": "date", "format": "epoch_second", "index": false },
					"deleted_date": { "type": "date", "format": "epoch_second", "index": true }
				}
			}
		}`,
		"chats": `{
			"settings": { "index": { "number_of_shards": 1, "number_of_replicas": 0 } },
			"mappings": {
				"properties": {
					"chat_body": { "type": "text", "index": true },
					"chat_by_user_id": { "type": "keyword", "index": true },
					"chat_by_user_full_name": { "type": "text", "index": true },
					"chat_by_profile": { "type": "keyword", "index": false },
					"chat_participants": {
						"type": "nested",
						"properties": {
							"user_name": { "type": "text", "index": true },
							"user_profile_object_key": { "type": "keyword", "index": false },
							"user_uuid": { "type": "keyword", "index": true }
						}
					},
					"chat_id": { "type": "keyword", "index": true },
					"chat_to_user_id": { "type": "keyword", "index": true },
					"chat_to_user_full_name": { "type": "text", "index": true },
					"chat_to_profile": { "type": "keyword", "index": false },
					"created_date": { "type": "date", "format": "epoch_second", "index": true },
					"updated_date": { "type": "date", "format": "epoch_second", "index": false },
					"deleted_date": { "type": "date", "format": "epoch_second", "index": true }
				}
			}
		}`,
		"comments": `{
			"settings": { "index": { "number_of_shards": 1, "number_of_replicas": 0 } },
			"mappings": {
				"properties": {
					"comment_body": { "type": "text", "index": true },
					"comment_by_user_id": { "type": "keyword", "index": false },
					"comment_by_user_full_name": { "type": "text", "index": true },
					"comment_by_profile": { "type": "keyword", "index": false },
					"comment_id": { "type": "keyword", "index": false },
					"comment_post_id": { "type": "keyword", "index": true },
					"comment_chat_id": { "type": "keyword", "index": true },
					"comment_chat_from_user_id": { "type": "keyword", "index": true },
					"comment_chat_from_user_full_name": { "type": "text", "index": true },
					"comment_chat_to_user_id": { "type": "keyword", "index": true },
					"comment_chat_grp_id": { "type": "keyword", "index": true },
					"comment_chat_participants": {
						"type": "nested",
						"properties": {
							"user_name": { "type": "text", "index": true },
							"user_profile_object_key": { "type": "keyword", "index": false },
							"user_uuid": { "type": "keyword", "index": true }
						}
					},
					"comment_doc_id": { "type": "keyword", "index": true },
					"comment_doc_title": { "type": "text", "index": true },
					"comment_channel_id": { "type": "keyword", "index": true },
					"comment_channel_name": { "type": "text", "index": true },
					"comment_project_id": { "type": "keyword", "index": true },
					"comment_project_name": { "type": "text", "index": true },
					"comment_task_id": { "type": "keyword", "index": true },
					"comment_doc_private": { "type": "boolean", "index": true },
					"comment_doc_reading_users": { "type": "keyword", "index": true },
					"comment_doc_editing_users": { "type": "keyword", "index": true },
					"comment_doc_commenting_users": { "type": "keyword", "index": true },
					"comment_doc_created_by_user_id": { "type": "keyword", "index": true },
					"created_date": { "type": "date", "format": "epoch_second", "index": true },
					"updated_date": { "type": "date", "format": "epoch_second", "index": false },
					"deleted_date": { "type": "date", "format": "epoch_second", "index": true }
				}
			}
		}`,
		"projects": `{
			"settings": { "index": { "number_of_shards": 1, "number_of_replicas": 0 } },
			"mappings": {
				"properties": {
					"project_id": { "type": "keyword", "index": true },
					"project_name": { "type": "text", "index": true },
					"project_team_id": { "type": "keyword", "index": true },
					"project_team_name": { "type": "text", "index": true },
					"created_date": { "type": "date", "format": "epoch_second", "index": true },
					"updated_date": { "type": "date", "format": "epoch_second", "index": false },
					"deleted_date": { "type": "date", "format": "epoch_second", "index": true }
				}
			}
		}`,
		"channels": `{
			"settings": { "index": { "number_of_shards": 1, "number_of_replicas": 0 } },
			"mappings": {
				"properties": {
					"ch_id": { "type": "keyword", "index": true },
					"ch_name": { "type": "text", "index": true },
					"created_date": { "type": "date", "format": "epoch_second", "index": true },
					"updated_date": { "type": "date", "format": "epoch_second", "index": false },
					"deleted_date": { "type": "date", "format": "epoch_second", "index": true }
				}
			}
		}`,
		"teams": `{
			"settings": { "index": { "number_of_shards": 1, "number_of_replicas": 0 } },
			"mappings": {
				"properties": {
					"team_id": { "type": "keyword", "index": true },
					"team_name": { "type": "text", "index": true },
					"created_date": { "type": "date", "format": "epoch_second", "index": true },
					"updated_date": { "type": "date", "format": "epoch_second", "index": false },
					"deleted_date": { "type": "date", "format": "epoch_second", "index": true }
				}
			}
		}`,
		"attachments": `{
			"settings": { "index": { "number_of_shards": 1, "number_of_replicas": 0 } },
			"mappings": {
				"properties": {
					"attachment_file_name": { "type": "text", "index": true },
					"attachment_object_key": { "type": "keyword", "index": false },
					"attachment_by_user_id": { "type": "keyword", "index": false },
					"attachment_by_user_full_name": { "type": "text", "index": true },
					"attachment_by_profile": { "type": "keyword", "index": false },
					"attachment_id": { "type": "keyword", "index": false },
					"attachment_channel_id": { "type": "keyword", "index": true },
					"attachment_channel_name": { "type": "text", "index": true },
					"attachment_chat_id": { "type": "keyword", "index": true },
					"attachment_chat_from_user_id": { "type": "keyword" , "index": true},
					"attachment_chat_from_user_name": { "type": "text" , "index": true},
					"attachment_chat_grp_id": { "type": "keyword", "index": true },
					"attachment_chat_participants": {
						"type": "nested",
						"properties": {
								"user_name": { "type": "text", "index": true },
								"user_profile_object_key": { "type": "keyword", "index": false },
								"user_uuid": { "type": "keyword", "index": true }
							}
					},
					"attachment_project_id": { "type": "keyword", "index": true },
					"attachment_project_name": { "type": "text", "index": true },
					"attachment_doc_id": { "type": "keyword", "index": true },
					"attachment_doc_title": { "type": "text", "index": true },
					"attachment_task_id": { "type": "keyword", "index": true },
					"attachment_post_id": { "type": "keyword", "index": true },
					"attachment_comment_id": { "type": "keyword", "index": true },
					"attachment_doc_private": { "type": "boolean", "index": true },
					"attachment_doc_reading_users": { "type": "keyword", "index": true },
					"attachment_doc_editing_users": { "type": "keyword", "index": true },
					"attachment_doc_commenting_users": { "type": "keyword", "index": true },
					"attachment_doc_created_by_user_id": { "type": "keyword", "index": true },
					"created_date": { "type": "date", "format": "epoch_second", "index": true },
					"deleted_date": { "type": "date", "format": "epoch_second", "index": true }
				}
			}
		}`,
		"docs": `{
			"settings": { "index": { "number_of_shards": 1, "number_of_replicas": 0 } },
			"mappings": {
				"properties": {
					"doc_title": { "type": "text", "index": true },
					"doc_body": { "type": "text", "index": true },
					"doc_snippet": { "type": "text", "index": true },
					"doc_uuid": { "type": "keyword", "index": true },
					"doc_created_by_user_id": { "type": "keyword", "index": true },
					"doc_created_by_user_full_name": { "type": "text", "index": true },
					"doc_created_by_profile": { "type": "keyword", "index": false },
					"doc_private": { "type": "boolean", "index": true },
					"doc_reading_users": { "type": "keyword", "index": true },
					"doc_editing_users": { "type": "keyword", "index": true },
					"doc_commenting_users": { "type": "keyword", "index": true },
					"created_date": { "type": "date", "format": "epoch_second", "index": true },
					"updated_date": { "type": "date", "format": "epoch_second", "index": false },
					"deleted_date": { "type": "date", "format": "epoch_second", "index": true }
				}
			}
		}`,
		"boards": `{
			"settings": { "index": { "number_of_shards": 1, "number_of_replicas": 0 } },
			"mappings": {
				"properties": {
					"board_title": { "type": "text", "index": true },
					"board_snippet": { "type": "text", "index": true },
					"board_uuid": { "type": "keyword", "index": true },
					"board_created_by_user_id": { "type": "keyword", "index": true },
					"board_created_by_user_full_name": { "type": "text", "index": true },
					"board_created_by_profile": { "type": "keyword", "index": false },
					"board_private": { "type": "boolean", "index": true },
					"board_reading_users": { "type": "keyword", "index": true },
					"board_editing_users": { "type": "keyword", "index": true },
					"board_commenting_users": { "type": "keyword", "index": true },
					"created_date": { "type": "date", "format": "epoch_second", "index": true },
					"updated_date": { "type": "date", "format": "epoch_second", "index": false },
					"deleted_date": { "type": "date", "format": "epoch_second", "index": true }
				}
			}
		}`,
		"tasks": `{
			"settings": { "index": { "number_of_shards": 1, "number_of_replicas": 0 } },
			"mappings": {
				"properties": {
					"task_name": { "type": "text", "index": true },
					"task_desc": { "type": "text", "index": true },
					"task_id": { "type": "keyword", "index": true },
					"task_project_id": { "type": "keyword", "index": true },
					"task_project_name": { "type": "text", "index": true },
					"task_assignee_user_id": { "type": "keyword", "index": true },
					"task_assignee_user_full_name": { "type": "text", "index": true },
					"task_assignee_profile": { "type": "keyword", "index": false },
					"task_created_by_user_id": { "type": "keyword", "index": true },
					"task_status": { "type": "keyword", "index": true },
					"task_priority": { "type": "keyword", "index": true },
					"task_label": { "type": "text", "index": true },
					"created_date": { "type": "date", "format": "epoch_second", "index": true },
					"updated_date": { "type": "date", "format": "epoch_second", "index": false },
					"deleted_date": { "type": "date", "format": "epoch_second", "index": true }
				}
			}
		}`,
		"users": `{
			"settings": { "index": { "number_of_shards": 1, "number_of_replicas": 0 } },
			"mappings": {
				"properties": {
					"user_id": { "type": "keyword", "index": true },
					"user_name": { "type": "text", "index": true },
					"user_full_name": { "type": "text", "index": true },
					"user_email": { "type": "keyword", "index": true },
					"user_profile_object_key": { "type": "keyword", "index": false },
					"created_date": { "type": "date", "format": "epoch_second", "index": true },
					"updated_date": { "type": "date", "format": "epoch_second", "index": false },
					"deleted_date": { "type": "date", "format": "epoch_second", "index": true }
				}
			}
		}`,
		"ai_embeddings": `{
			"settings": {
				"index": {
					"knn": true,
					"number_of_shards": 1,
					"number_of_replicas": 0
				}
			},
			"mappings": {
				"properties": {
					"embedding": { "type": "knn_vector", "dimension": 768 },
					"content_text": { "type": "text", "index": true },
					"content_type": { "type": "keyword", "index": true },
					"content_uuid": { "type": "keyword", "index": true },
					"created_date": { "type": "date", "format": "epoch_second", "index": true },
					"deleted_date": { "type": "date", "format": "epoch_second", "index": true },

					"channel_uuid": { "type": "keyword", "index": true },
					"channel_name": { "type": "text", "index": true },

					"chat_by_user_id": { "type": "keyword", "index": true },
					"chat_to_user_id": { "type": "keyword", "index": true },
					"chat_participant_uuids": { "type": "keyword", "index": true },
					"chat_grp_id": { "type": "keyword", "index": true },

					"doc_private": { "type": "boolean", "index": true },
					"doc_created_by_user_id": { "type": "keyword", "index": true },
					"doc_reading_users": { "type": "keyword", "index": true },
					"doc_editing_users": { "type": "keyword", "index": true },
					"doc_commenting_users": { "type": "keyword", "index": true },

					"project_uuid": { "type": "keyword", "index": true },
					"task_assignee_user_id": { "type": "keyword", "index": true },
					"task_created_by_user_id": { "type": "keyword", "index": true },

					"author_uuid": { "type": "keyword", "index": true },
					"author_name": { "type": "text", "index": true }
				}
			}
		}`,
	}

	for index, body := range indicesToCreate {
		existsResp, err := OpenSearchClient.Indices.Exists(ctx, opensearchapi.IndicesExistsReq{
			Indices: []string{index},
		})
		if err == nil && existsResp.StatusCode == http.StatusOK {
			continue // Index already exists
		}

		_, err = OpenSearchClient.Indices.Create(ctx, opensearchapi.IndicesCreateReq{
			Index: index,
			Body:  strings.NewReader(body),
		})

		if err != nil {
			helpers.LogErrorWithContext(ctx, "opensearchInit/connectOpenSearch Failed to create index %s err:= %+v", index, err)
			return err
		}
	}

	return nil
}
