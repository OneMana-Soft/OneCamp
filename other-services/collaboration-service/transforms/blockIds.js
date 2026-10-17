// Block ids for docs written before blocks had them (docs programme §4 #4, A1).
//
// The browser gives a block its id as it is made or first edited, in that
// tab's own transactions (the web's extensions/block-id). A doc written before
// that has blocks with none, and threads, proposals and the Live Doc Writer
// can't find them. So as a doc loads, the server gives each block that has no
// id (or another block's id: a copy) one of its own, in one transaction: an
// edit like any other, sent to every tab, buffered and stored.
//
// Idempotent per lineage: an id is made from what never changes about the
// block in this copy of the doc (the doc, its lineage, and the Yjs item that
// holds the element). A second run finds nothing to do; two servers doing it
// at once write the same ids, so nothing flips. Only what a block lacks is
// written: its content is never touched, so nothing can double.

import { createHash } from 'node:crypto'
import { getSchema } from '@tiptap/core'
import * as Y from 'yjs'
import { BLOCK_ID_ATTR } from '../schema/blockId.js'
import { FIELD, epochIn } from '../lineage.js'

// blockTypesIn is the node types that take a block id, read from the schema.
export const blockTypesIn = (schema) =>
    new Set(Object.values(schema.nodes).filter((t) => t.spec.attrs?.[BLOCK_ID_ATTR] !== undefined).map((t) => t.name))

// idFor is a block's id: a UUID (version 4 in shape, as the browser's are)
// from the doc, its lineage and the element's own Yjs item.
export function idFor(docName, lineage, element) {
    const { client, clock } = element._item.id
    const b = createHash('sha256').update(`${docName}\n${lineage}\n${client}:${clock}`, 'utf8').digest()
    b[6] = (b[6] & 0x0f) | 0x40
    b[8] = (b[8] & 0x3f) | 0x80
    const h = b.subarray(0, 16).toString('hex')
    return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`
}

// blocksNeedingIds is every block, in document order, that has no id or has
// the id of a block before it (the later one is the copy).
export function blocksNeedingIds(fragment, types) {
    const seen = new Set()
    const todo = []
    const walk = (parent) => {
        for (const child of parent.toArray()) {
            if (!(child instanceof Y.XmlElement)) continue
            if (types.has(child.nodeName)) {
                const id = child.getAttribute(BLOCK_ID_ATTR)
                if (typeof id !== 'string' || id === '' || seen.has(id)) todo.push(child)
                else seen.add(id)
            }
            walk(child)
        }
    }
    walk(fragment)
    return todo
}

// assignBlockIds gives every block that needs one its id, in one
// transaction (with `origin`, when given), and says how many it gave.
export function assignBlockIds(ydoc, { docName, types, origin = null }) {
    const todo = blocksNeedingIds(ydoc.getXmlFragment(FIELD), types)
    if (todo.length === 0) return 0
    const lineage = epochIn(ydoc) || ''
    ydoc.transact(() => {
        for (const element of todo) element.setAttribute(BLOCK_ID_ATTR, idFor(docName, lineage, element))
    }, origin)
    return todo.length
}

// createBlockIdsTransform is what the server runs as a doc loads (its
// afterLoadDocument hook): the ids go into the loaded doc as one edit, with an
// origin of the server's own, so Hocuspocus treats it as an edit like any
// other (sent to every tab, buffered, and saved with the next save, coalesced
// with what people type). If the server stops before that save, the next load
// gives the same ids again.
export function createBlockIdsTransform({ extensions, log = console }) {
    const types = blockTypesIn(getSchema(extensions))
    // Whose edit it is: the server's ("unknown" is never counted as an editor).
    const origin = { context: { user: { id: 'unknown', name: 'Block ids' } }, socketId: 'server' }
    return {
        types,
        origin,
        give(docName, ydoc) {
            const given = assignBlockIds(ydoc, { docName, types, origin })
            if (given > 0) log.log(`[Collab] ${docName}: gave ${given} block${given === 1 ? '' : 's'} an id`)
            return given
        },
    }
}
