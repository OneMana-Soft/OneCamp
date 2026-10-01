package adapter

type GlobalSearchInfo struct {
	SearchText string `json:"global_search_text,omitempty"`
	TimeStamp  int64  `json:"global_search_time_stamp,omitempty"`
}
