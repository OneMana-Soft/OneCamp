package adapter

type TranscriptInput struct {
	RoomName            string `json:"room_name"`
	ParticipantIdentity string `json:"participant_identity"`
	Text                string `json:"text"`
	Timestamp           int64  `json:"timestamp"`
	EgressID            string `json:"egress_id"`
	// OffsetMs is the utterance start in milliseconds from recording start,
	// measured in the producer's own clock domain (browser in frontend mode,
	// agent in backend mode). It is the skew-free seek anchor used by playback.
	// Nil (field absent) means "unknown — fall back to absolute-timestamp
	// subtraction" for older agents that don't send it.
	OffsetMs *int64 `json:"offset_ms,omitempty"`
}
