// "Saved" means stored (invariant I8).
//
// The save dot used to say "saved" when this service held a change in memory.
// The database write comes 5 to 30 seconds later, and a write that failed never
// reached the dot. Now, after every save the API confirms, the doc's open tabs
// get a stateless message:
//
//   {"type":"persisted","at":1760262067000,"sv":"<base64>","lineage":"…"}
//
// `at` is when it was stored, in ms since the epoch: the doc session reads it
// as a number (web lib/collabSession.ts, A2), and so does the save dot.
// `sv` is the doc's Yjs state vector at the moment the saved state was encoded:
// for every client that ever edited it, how much of that client's work the stored
// copy holds. A tab knows its own client id and how much it has written, so it
// knows exactly when everything it typed is stored, whatever else others did
// meanwhile (web components/docEditor/docSaveState.ts reads this message).
// `lineage` is the copy of the doc that was stored (lineage.js).
//
// The same comparison tells a Live Doc Writer op whether its write is stored
// (live/ops/write.js).

import * as Y from 'yjs'

export const PERSISTED_TYPE = 'persisted'

const b64 = (u8) => Buffer.from(u8).toString('base64')

export function persistedMessage({ at, stateVector, lineage = '' }) {
    return JSON.stringify({ type: PERSISTED_TYPE, at: Math.round(at), sv: b64(stateVector), lineage })
}

const decode = (sv) => {
    if (!sv) return new Map()
    try {
        return Y.decodeStateVector(sv instanceof Uint8Array ? sv : Buffer.from(sv, 'base64'))
    } catch {
        return null
    }
}

// covers is whether a stored state (its state vector `stored`) holds everything
// in `wanted`: for every client, the stored clock reaches the wanted one.
export function covers(stored, wanted) {
    const s = decode(stored)
    const w = decode(wanted)
    if (!s || !w) return false
    for (const [client, clock] of w) {
        if ((s.get(client) || 0) < clock) return false
    }
    return true
}

// sameState is whether two state vectors name the same state.
export const sameState = (a, b) => !!a && !!b && covers(a, b) && covers(b, a)

// createPersistence remembers, per doc, the last state the API stored, so a
// write can tell whether it is stored and a save whether there is anything new
// to store. Kept in memory: after a restart the first save of a doc is a full
// one, and the API tells an unchanged body apart itself.
export function createPersistence() {
    const last = new Map() // name -> { at, stateVector, hash, lineage }
    return {
        // stored records a save the API confirmed (hash is the body's, or null
        // when the body was kept as it was).
        stored(name, { at = Date.now(), stateVector, hash = null, lineage = '' }) {
            last.set(name, { at, stateVector, hash, lineage })
        },
        get: (name) => last.get(name) || null,
        // covers is whether the last stored state of `name` holds `stateVector`.
        covers(name, stateVector) {
            const l = last.get(name)
            return !!l && covers(l.stateVector, stateVector)
        },
        forget(name) {
            last.delete(name)
        },
    }
}
