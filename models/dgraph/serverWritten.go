package models

// ServerWritten is whether the server wrote this message rather than a person:
// a system line, or a call's post (calls CALLS-3b). Nobody edits or forwards
// one; its kind says which it is. Any kind counts, so a kind added later for a
// person's own message would be refused too until it's named here: it fails
// closed. Pure.
func (p *DgraphPost) ServerWritten() bool { return p != nil && p.Kind != "" }

// ServerWritten is ServerWritten for a DM's or a group's message. Pure.
func (c *DgraphChat) ServerWritten() bool { return c != nil && c.Kind != "" }
