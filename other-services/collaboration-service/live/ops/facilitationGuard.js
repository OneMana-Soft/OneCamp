// A board's facilitation (V-462): its timer, its voting round and its private
// round live in the map "facilitation", and they are its members' to run. A
// call's guest whom the call lets draw holds a writable connection, so a
// crafted guest client could start or end a round, and ending a private round
// would reveal its held notes early. Every guest connection's write to the map
// is put back, failing closed; members' writes stand. Viewers' connections are
// read-only already (the relay refuses their updates).

import { putBackSocketWrites } from './mapGuard.js'
import { byPerson } from './authorGuard.js'

export const FACILITATION = 'facilitation'

// The origin of this service's put-backs of the map.
export const FACILITATION_ORIGIN = Object.freeze({ facilitationGuard: true })

// A round names who started it (byId, byName) and the timer who set it
// (byName): a member starts or sets one as themselves (authorGuard.js, V-965).
const asThemselves = byPerson('byId', 'byName')

// guardFacilitation keeps a board's facilitation from its guests' sockets,
// and from any socket whose context says it can't edit (failing closed), and
// its members' rounds as their own; heard hears of a round put back as
// someone else's (authorGuard.js toldOf).
export function guardFacilitation(doc, heard) {
    putBackSocketWrites(doc, doc.getMap(FACILITATION), FACILITATION_ORIGIN, (context, ...change) =>
        context.guest === true || context.access?.canEdit === false || asThemselves(context, ...change), heard)
}
