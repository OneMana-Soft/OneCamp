package adapter

type GlobalSearchInfo struct {
	SearchText string `json:"global_search_text,omitempty"`
	// After is the cursor of the page wanted next (a page's next), or empty
	// for the first page.
	After string `json:"after,omitempty"`
	// One conversation to search in, at most: a channel by its id, a DM by
	// the other person's id, a group by its id. Empty searches everything.
	Channel string `json:"channel,omitempty"`
	DM      string `json:"dm,omitempty"`
	Group   string `json:"group,omitempty"`
}
