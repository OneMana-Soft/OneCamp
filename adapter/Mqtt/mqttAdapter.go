package adapter

type OutputMqttConfig struct {
	Topics   []string `json:"topics,omitempty"`
	Username string   `json:"username,omitempty"`
	Password string   `json:"password,omitempty"`
	WsUrl    string   `json:"ws_url,omitempty"`
	ClientId string   `json:"clientId,omitempty"`
}
