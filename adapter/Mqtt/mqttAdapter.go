package adapter

type OutputMqttConfig struct {
	Topics []string `json:"topics,omitempty"`
	// ConversationTopics is each channel's, DM's and group's topics in Topics,
	// by its key (business/Mqtt conversationKey).
	ConversationTopics map[string][]string `json:"conversation_topics,omitempty"`
	Username           string              `json:"username,omitempty"`
	Password           string              `json:"password,omitempty"`
	WsUrl              string              `json:"ws_url,omitempty"`
	ClientId           string              `json:"clientId,omitempty"`
}
