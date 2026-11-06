// A board's facilitation (V-462): its timer, its voting round and its private
// round live in the map "facilitation", and they are its members' to run. A
// call's guest whom the call lets draw holds a writable connection, so a
// crafted guest client could start or end a round, and ending a private round
// would reveal its held notes early. Every guest connection's write to the map
// is put back, failing closed; members' writes stand. Viewers' connections are
// read-only already (the relay refuses their updates).

import { putBackSocketWrites } from './mapGuard.js'

export const FACILITATION = 'facilitation'

// The origin of this service's put-backs of the map.
export const FACILITATION_ORIGIN = Object.freeze({ facilitationGuard: true })

// guardFacilitation keeps a board's facilitation from its guests' sockets,
// and from any socket whose context says it can't edit (failing closed).
export function guardFacilitation(doc) {
    putBackSocketWrites(doc, doc.getMap(FACILITATION), FACILITATION_ORIGIN, (context) => context.guest === true || context.access?.canEdit === false)
}
