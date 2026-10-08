package Calendar

import "time"

type CreateOrUpdateEventInput struct {
	Title                string   `json:"title" binding:"required"`
	Description          string   `json:"description"`
	StartTime            string   `json:"startTime" binding:"required"`
	EndTime              string   `json:"endTime" binding:"required"`
	Participants         []string `json:"participants"`
	SyncToGoogleCalendar bool     `json:"syncToGoogleCalendar"`
	// IsFocus pauses the creator's notifications while the event runs. A
	// pointer: an update that leaves it out leaves focus as it was.
	IsFocus *bool `json:"isFocus,omitempty"`
	// IsAway marks the creator away (time off) while it runs; the workload
	// takes those working days off their capacity. Away is never also focus.
	IsAway *bool `json:"isAway,omitempty"`
}

type OutputEvent struct {
	EventUuid    string     `json:"eventUuid"`
	Title        string     `json:"title"`
	Description  string     `json:"description"`
	StartTime    *time.Time `json:"startTime"`
	EndTime      *time.Time `json:"endTime"`
	CreatedBy    string     `json:"createdByUuid"`
	Participants []string   `json:"participants"`
	IsFocus      bool       `json:"isFocus"`
	IsAway       bool       `json:"isAway"`
}
