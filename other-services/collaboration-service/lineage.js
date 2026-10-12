// One copy of a doc, however often it is rebuilt from its HTML.
//
// WHY THIS EXISTS. When a doc has no saved Yjs state that matches its body
// (after `demoseed --refresh`, a restore, an API write of the body), the server
// rebuilds it from the HTML. Each rebuild used to make a new Yjs history: the
// same text under new item ids. A tab still holding the earlier copy merged
// both on its next reconnect, and the doc held one more full copy each time
// (11 Oct 2026: "Q4 launch plan" went from 7 blocks to 77 in six minutes on
// the demo, one copy per reconnect).
//
// Three parts here:
//   - rebuildFromBody builds the doc with a client id and clock fixed by the
//     doc id and the body's hash, so the same body always gives byte-identical
//     items and merging two rebuilds changes nothing. It stamps the rebuild's
//     epoch (the same hash) in the doc's `meta` map, which the web app
//     remembers: a different epoch means a different body, and the tab reloads
//     instead of merging (hooks/useCollaborationProvider.ts);
//   - the caller saves the rebuilt state at once (collabServer.js), so later
//     loads open it instead of rebuilding;
//   - bringsSecondCopy tells, from an incoming update, that a client is sending
//     a whole other copy of the doc (an earlier lineage: content inserted at the
//     root with no neighbours, into a doc that already has content). The server
//     then drops that client's edits and tells it to reload; it also keeps such
//     updates out of the replay buffer.

import { createHash } from 'node:crypto'
import { getSchema } from '@tiptap/core'
import { prosemirrorJSONToYXmlFragment } from 'y-prosemirror'
import * as Y from 'yjs'
import { bodyHash } from './docState.js'
import { htmlToDoc } from './preserved.js'

export const FIELD = 'default'
export const META = 'meta'

// epochOf is the identity of the copy built from a body: the doc id and the
// body's hash, hashed.
export const epochOf = (docId, body) =>
    createHash('sha256').update(`${docId}\n${bodyHash(body)}`, 'utf8').digest('hex').slice(0, 32)

// seedOf is the client id a rebuild writes with: 32 bits of its epoch, never 0.
export const seedOf = (epoch) => (parseInt(epoch.slice(0, 8), 16) >>> 0) || 1

export const epochIn = (ydoc) => {
    const v = ydoc.getMap(META).get('epoch')
    return typeof v === 'string' ? v : null
}

// rebuildFromBody is the doc built from its HTML body, the same bytes every
// time for the same doc and body.
export function rebuildFromBody(docId, body, extensions) {
    const { json, extensions: withKept } = htmlToDoc(body, extensions)
    const epoch = epochOf(docId, body)
    const ydoc = new Y.Doc()
    ydoc.clientID = seedOf(epoch)
    ydoc.transact(() => {
        prosemirrorJSONToYXmlFragment(getSchema(withKept), json, ydoc.getXmlFragment(FIELD))
        ydoc.getMap(META).set('epoch', epoch)
    })
    return { ydoc, epoch }
}

// bringsSecondCopy reports whether applying `update` to `ydoc` would add a
// second copy of the doc: an item inserted at the top level of the body with
// nothing before or after it (only the first block of a doc built from scratch
// looks like that), that the doc does not have yet, while the doc already has
// content. Ordinary edits, offline ones included, always sit next to content
// the doc already holds.
export function bringsSecondCopy(update, ydoc) {
    if (!update || update.length === 0) return false
    if (ydoc.getXmlFragment(FIELD).length === 0) return false
    let structs
    try {
        structs = Y.decodeUpdate(update).structs
    } catch (e) {
        return false
    }
    for (const s of structs) {
        if (!(s instanceof Y.Item)) continue
        if (s.parent !== FIELD || s.parentSub != null) continue
        if (s.origin !== null || s.rightOrigin !== null) continue
        if (Y.getState(ydoc.store, s.id.client) > s.id.clock) continue
        return true
    }
    return false
}

// syncUpdateOf is the Yjs update a Hocuspocus message carries (a sync step 2
// or an update), or null for any other message.
export function syncUpdateOf(message) {
    let i = 0
    const varUint = () => {
        let n = 0
        let mult = 1
        while (i < message.length) {
            const b = message[i++]
            n += (b & 0x7f) * mult
            if (b < 0x80) return n
            mult *= 128
        }
        throw new Error('truncated')
    }
    try {
        const nameLength = varUint() // the document name, skipped
        i += nameLength
        if (varUint() !== 0) return null // not a sync message
        const type = varUint()
        if (type !== 1 && type !== 2) return null // step 1 carries no content
        const length = varUint()
        if (i + length > message.length) return null
        return message.subarray(i, i + length)
    } catch (e) {
        return null
    }
}

// The stateless message a client sending a second copy gets. The web app drops
// its copy and opens the doc again, keeping any unsent edits aside.
export const EPOCH_CHANGED_MESSAGE = JSON.stringify({ type: 'epoch-changed' })
