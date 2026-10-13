package business

// Who may change a channel's privacy, and what everyone is told when it does.
//
// Any of a channel's moderators could make a private channel public with one
// switch, and with it its whole history and files readable by everyone in the
// workspace, telling nobody. Opening a private channel up is now a workspace
// admin's decision (Slack's rule: only owners and admins may); closing one
// stays a moderator's. Either way a line in the channel says who changed it
// and what it means.

import (
	"fmt"
	"html"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// PrivacyChange is what an update does to a channel's privacy.
type PrivacyChange int

const (
	PrivacyUnchanged PrivacyChange = iota
	PrivacyOpened                  // private to public
	PrivacyClosed                  // public to private
)

// PrivacyChangeOf is what an update asking for private (the requested value)
// does to channel, as it stands. An archive keeps the channel's privacy as it
// is asked to be, and is no change of it. A channel whose privacy didn't load
// is taken as private, so opening it is asked of an admin. Pure.
func PrivacyChangeOf(channel *dgraphStruct.DgraphChannel, private bool) PrivacyChange {
	wasPrivate := channel == nil || channel.IsPrivate == nil || *channel.IsPrivate
	switch {
	case wasPrivate && !private:
		return PrivacyOpened
	case !wasPrivate && private:
		return PrivacyClosed
	}
	return PrivacyUnchanged
}

// MayChangePrivacy reports whether someone may make change: anyone who may
// edit the channel closes it (its moderators), only a workspace admin opens
// it. Pure.
func MayChangePrivacy(change PrivacyChange, workspaceAdmin bool) bool {
	return change != PrivacyOpened || workspaceAdmin
}

// PrivacyLine is the line posted in the channel when who changes its
// privacy, as HTML; "" for no change. Pure.
func PrivacyLine(change PrivacyChange, who string) string {
	who = html.EscapeString(who)
	switch change {
	case PrivacyOpened:
		return fmt.Sprintf("<p><strong>%s</strong> made this channel public. Everyone in the workspace can now find it and read its history and files.</p>", who)
	case PrivacyClosed:
		return fmt.Sprintf("<p><strong>%s</strong> made this channel private. Only its members can find it and read it now.</p>", who)
	}
	return ""
}
