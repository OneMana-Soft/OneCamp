package models

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

type WebhookLog struct {
	Id             uuid.UUID `json:"id"`
	WebhookId      uuid.UUID `json:"webhook_id"`
	EventType      string    `json:"event_type"`
	RequestBody    *string   `json:"request_body,omitempty"`
	ResponseStatus *int      `json:"response_status,omitempty"`
	ResponseBody   *string   `json:"response_body,omitempty"`
	ErrorMessage   *string   `json:"error_message,omitempty"`
	DurationMs     *int      `json:"duration_ms,omitempty"`
	Success        bool      `json:"success"`
	CreatedAt      time.Time `json:"created_at"`
}

func CreateWebhookLog(query string, id uuid.UUID, webhookId uuid.UUID, eventType string, requestBody *string, responseStatus *int, responseBody *string, errorMessage *string, durationMs *int, success bool, createdAt time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		id,
		webhookId,
		eventType,
		requestBody,
		responseStatus,
		responseBody,
		errorMessage,
		durationMs,
		success,
		createdAt,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateWebhookLog Failed to create webhook log err: %+v",
			err)
		return err
	}

	return nil
}

func GetWebhookLogsByWebhookId(query string, webhookId uuid.UUID, limit int, offset int) ([]*WebhookLog, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, webhookId, limit, offset)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetWebhookLogsByWebhookId Failed to get webhook logs err: %+v",
			err)
		return nil, err
	}
	defer rows.Close()

	var logs []*WebhookLog
	for rows.Next() {
		var log WebhookLog
		if err := rows.Scan(
			&log.Id,
			&log.WebhookId,
			&log.EventType,
			&log.RequestBody,
			&log.ResponseStatus,
			&log.ResponseBody,
			&log.ErrorMessage,
			&log.DurationMs,
			&log.Success,
			&log.CreatedAt,
		); err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/GetWebhookLogsByWebhookId Failed to scan log row err: %+v",
				err)
			return nil, err
		}
		logs = append(logs, &log)
	}
	// Iteration can stop on a mid-query failure (dropped connection, server-side
	// error) rather than on end-of-rows. Without it this returns a PARTIAL result
	// with a nil error, and the caller cannot tell truncated data from a short list.
	if err := rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/WebhookLog rows iteration failed err: %+v", err)
		return nil, err
	}

	return logs, nil
}

func GetWebhookLogsGlobal(query string, webhookId *uuid.UUID, success *bool, fromDate *time.Time, toDate *time.Time, limit int, offset int) ([]*WebhookLog, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, webhookId, success, fromDate, toDate, limit, offset)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetWebhookLogsGlobal Failed to get webhook logs err: %+v",
			err)
		return nil, err
	}
	defer rows.Close()

	var logs []*WebhookLog
	for rows.Next() {
		var log WebhookLog
		if err := rows.Scan(
			&log.Id,
			&log.WebhookId,
			&log.EventType,
			&log.RequestBody,
			&log.ResponseStatus,
			&log.ResponseBody,
			&log.ErrorMessage,
			&log.DurationMs,
			&log.Success,
			&log.CreatedAt,
		); err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/GetWebhookLogsGlobal Failed to scan log row err: %+v",
				err)
			return nil, err
		}
		logs = append(logs, &log)
	}
	// Iteration can stop on a mid-query failure (dropped connection, server-side
	// error) rather than on end-of-rows. Without it this returns a PARTIAL result
	// with a nil error, and the caller cannot tell truncated data from a short list.
	if err := rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/WebhookLog rows iteration failed err: %+v", err)
		return nil, err
	}

	return logs, nil
}

func DeleteOldWebhookLogs(query string, cutoffDate time.Time) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	result, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, cutoffDate)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/DeleteOldWebhookLogs Failed to delete old logs err: %+v",
			err)
		return 0, err
	}

	rowsAffected, _ := result.RowsAffected()
	return rowsAffected, nil
}
