package adapter

type GlobalSearchInfo struct {
	SearchText string `json:"global_search_text,omitempty"`
	// After is the cursor of the page wanted next (a page's next), or empty
	// for the first page.
	After string `json:"after,omitempty"`
}
