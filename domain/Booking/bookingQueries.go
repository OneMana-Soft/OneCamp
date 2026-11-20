// Package domain holds the booking pages' query text; models/postgres/Booking
// runs it.
package domain

import "github.com/google/uuid"

// PageLockKey is the lock a booking page's bookings are taken and moved
// under, for the rest of the transaction (pgexec.LockXact): one at a time per
// page.
func PageLockKey(page uuid.UUID) string { return "booking:" + page.String() }

// Clash is whether an active booking of page $1 overlaps $2..$3, other than
// booking $4 (NULL for none): the one being moved is no clash with itself.
const Clash = `
	SELECT EXISTS (SELECT 1 FROM bookings WHERE page_id = $1 AND cancelled_at IS NULL
	                 AND starts_at < $3 AND ends_at > $2 AND id IS DISTINCT FROM $4)`

// BookingByID is one booking by its id, for a job that names it (its guest's
// reminder): the model selects its own columns before it.
const BookingByID = `FROM bookings WHERE id = $1`

// QuestionsKept ends the WHERE of SavePage's update: every question id the
// edit gives back ($14, text[]) is one the page asks now. A question's id is
// the server's: never one a client made up, or another page's.
const QuestionsKept = `
			   AND $14::text[] <@ ARRAY(SELECT q->>'id' FROM jsonb_array_elements(booking_pages.questions) q)`

// EligibleHosts is which of $1 (uuid[]) can host a team page: active human
// members, never a bot, someone from outside the workspace or one who left.
const EligibleHosts = `SELECT id FROM users WHERE id = ANY($1::uuid[])
	AND deleted_at IS NULL AND is_bot = false AND is_external = false`

// InviteHosts invites $2 (uuid[]) to host page $1: someone new is invited,
// someone who left is invited again (accepted_at and left_at cleared), and
// someone invited or hosting already is left as they are. It returns who was
// invited now.
const InviteHosts = `INSERT INTO booking_page_hosts (page_id, user_id)
	SELECT $1, u FROM unnest($2::uuid[]) u
	ON CONFLICT (page_id, user_id) DO UPDATE SET invited_at = now(), accepted_at = NULL, left_at = NULL
	 WHERE booking_page_hosts.left_at IS NOT NULL
	RETURNING user_id`

// DropHosts takes every host of page $1 not among $2 (uuid[]) off it: their
// row stays, with left_at.
const DropHosts = `UPDATE booking_page_hosts SET left_at = now()
	WHERE page_id = $1 AND left_at IS NULL AND NOT (user_id = ANY($2::uuid[]))`

// HostsOfPages is every host row of pages $1 (uuid[]) whose member is still
// in the workspace, in the order they were invited: someone who left it is no
// host, named, counted or booked (CR-452).
const HostsOfPages = `SELECT h.page_id, h.user_id, h.invited_at, h.accepted_at, h.left_at
	FROM booking_page_hosts h JOIN users u ON u.id = h.user_id AND u.deleted_at IS NULL
	WHERE h.page_id = ANY($1::uuid[]) ORDER BY h.invited_at, h.user_id`

// HostsLeaveGonePage is a step after a `gone` step that deleted a page
// (RETURNING its id): every host still on it leaves, softly, so a page made
// again on its address asks them afresh (InviteHosts) rather than finding
// them accepted, and nobody stays on a page that is gone.
const HostsLeaveGonePage = `hosts_left AS (UPDATE booking_page_hosts h SET left_at = now()
	FROM gone WHERE h.page_id = gone.id AND h.left_at IS NULL)`

// Hosting is the live pages member $1 is invited to or hosts, through their
// own rows alone: newest invitation first.
const Hosting = `SELECT p.id, p.user_id, p.title, p.slug, h.invited_at, h.accepted_at, h.left_at
	FROM booking_page_hosts h JOIN booking_pages p ON p.id = h.page_id
	WHERE h.user_id = $1 AND h.left_at IS NULL AND p.deleted_at IS NULL
	ORDER BY h.invited_at DESC`

// hostRow is member $2's own row on live page $1, still on it: what accepting
// and leaving change, returning the page's owner.
const hostRow = `
	FROM booking_pages p WHERE h.page_id = $1 AND h.user_id = $2 AND h.left_at IS NULL
	 AND p.id = h.page_id AND p.deleted_at IS NULL RETURNING p.user_id`

// AcceptHost is member $2 accepting page $1 (once: a second accept keeps the
// first's time).
const AcceptHost = `UPDATE booking_page_hosts h SET accepted_at = COALESCE(h.accepted_at, now())` + hostRow

// LeaveHost is member $2 leaving page $1.
const LeaveHost = `UPDATE booking_page_hosts h SET left_at = now()` + hostRow
