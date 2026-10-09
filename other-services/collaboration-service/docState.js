// Which state a document opens from.
//
// The server rebuilt a document's Yjs state from its stored HTML every time it
// loaded it, and to Yjs that is a new document: the same text under new item
// ids. A browser still holding the old state (it stayed open while the document
// was unloaded and loaded again, or across a server restart) syncs both, and
// Yjs keeps both: every paragraph twice.
//
// So the server saves its Yjs state beside the body, and the backend stores a
// hash of the body it saved with it. The document opens from that state while
// the stored body is still that body. Anything that changes the body another
// way (a version restored, an import, an edit through the API) leaves the hash
// behind, and the document is rebuilt from the body as before.

import { createHash } from 'node:crypto'
import * as Y from 'yjs'

// bodyHash is the hash the backend stores beside a saved state: SHA-256 of the
// body's UTF-8 bytes, in hex.
export const bodyHash = (body) => createHash('sha256').update(body, 'utf8').digest('hex')

// savedStateFor is the document opened from its saved state, or null when it
// must be rebuilt from the body: there is no saved state, the body has changed
// since it was saved, or the state doesn't decode to a document with content.
export function savedStateFor(body, stateB64, hash) {
    if (typeof body !== 'string' || body.trim() === '') return null
    if (typeof stateB64 !== 'string' || stateB64 === '' || typeof hash !== 'string' || hash === '') return null
    if (bodyHash(body) !== hash) return null
    try {
        const ydoc = new Y.Doc()
        Y.applyUpdate(ydoc, Buffer.from(stateB64, 'base64'))
        if (ydoc.getXmlFragment('default').length === 0) return null
        return ydoc
    } catch (e) {
        return null
    }
}

// encodeState is a document's whole state, as savedStateFor reads it back.
export const encodeState = (ydoc) => Buffer.from(Y.encodeStateAsUpdate(ydoc)).toString('base64')
