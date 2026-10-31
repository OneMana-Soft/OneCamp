// The Live Doc Writer's ops, by name (docs programme, PROGRAM.md §4 #9; A4 built
// the first ops, A9 adds applyProposal, C8 the synced-block ops).
//
// Every content change to an existing doc goes through the copy this service
// holds: an op runs as ONE Yjs transaction on the live document, so whoever has
// it open sees the change arrive and nothing they type is overwritten. The API
// calls an op on the internal port (live/server.js) after deciding the caller may
// make it; the op trusts the attribution the API sends.
//
// An op is `async ({ docName, body, hocuspocus, deps }) => result`, and throws an
// error with a `status` to refuse (404 for no such block, 409 for a stale
// proposal). deps is collabServer.js's `live`.
//
// One entry per op, added by the integrator when its package merges (PROGRAM.md
// §3.6), with its import beside it.

import { append, insertAfter, replaceAll, replaceBlocks, seed } from './ops/content.js'
import { read, render } from './ops/read.js'
import { closePeople, reauth } from './ops/reauth.js'
import { applyProposal } from './ops/applyProposal.js' // A9
import { setTaskChecks } from './ops/taskChecks.js' // C1
import { placeSubpage, removeSubpage } from './ops/subpages.js' // C7a
import { guestsBegin, guestsSnapshot, undoGuests } from './ops/guestStrokes.js' // calls B

export const LIVE_ROUTES = new Map([
    ['append', append],
    ['insertAfter', insertAfter],
    ['replaceBlocks', replaceBlocks],
    ['replaceAll', replaceAll],
    ['seed', seed],
    ['reauth', reauth],
    ['read', read],
    ['render', render],
    ['applyProposal', applyProposal], // A9: edit proposals
    ['setTaskChecks', setTaskChecks], // C1: a task done elsewhere ticks its to-dos
    ['removeSubpage', removeSubpage], // C7a: a moved page's block leaves the parent it left
    ['placeSubpage', placeSubpage], // C7a: and lands at the end of the one it went to
])

// A board's ops, at /live/boards/<uuid>/<op> (its document is board:<uuid>):
// asking again who may be on it, and a call's guests' drawing (calls B).
export const BOARD_ROUTES = new Map([
    ['reauth', reauth],
    ['guestsSnapshot', guestsSnapshot],
    ['guestsBegin', guestsBegin],
    ['undoGuests', undoGuests],
])

// Ops about people rather than one doc or board, at /live/people/<op>: who is
// in the body.
export const PEOPLE_ROUTES = new Map([
    ['close', closePeople],
])
