package Calendar

import "time"

type CreateOrUpdateEventInput struct {
	Title                string   `json:"title" binding:"required"`
	Description          string   `json:"description"`
	StartTime            string   `json:"startTime" binding:"required"`
	EndTime              string   `json:"endTime" binding:"required"`
	Participants         []string `json:"participants"`
	SyncToGoogleCalendar bool     `json:"syncToGoogleCalendar"`
}

type OutputEvent struct {
	EventUuid    string     `json:"eventUuid"`
	Title        string     `json:"title"`
	Description  string     `json:"description"`
	StartTime    *time.Time `json:"startTime"`
	EndTime      *time.Time `json:"endTime"`
	CreatedBy    string     `json:"createdByUuid"`
	Participants []string   `json:"participants"`
}
