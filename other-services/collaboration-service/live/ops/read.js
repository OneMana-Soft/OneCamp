// Reading a doc as it is now, and writing a saved state as HTML.

import * as Y from 'yjs'
import { TiptapTransformer } from '@hocuspocus/transformer'
import { bodyHash } from '../../docState.js'
import { epochIn } from '../../lineage.js'
import { renderDocHTML } from '../../preserved.js'
import { FIELD } from './blocks.js'
import { currentHTML, openDoc, refuse } from './write.js'

// read answers the live doc's body, its hash and its lineage: what an export,
// a version taken on request or a proposal's base reads, typing included (the
// stored body can be 30 seconds behind). A doc not open is loaded for it.
export async function read({ docName, hocuspocus, deps }) {
    const id = docName.toLowerCase()
    const open = !!hocuspocus.documents?.has(id)
    const connection = await openDoc(hocuspocus, id)
    try {
        const doc = connection.document
        const html = currentHTML(doc, deps.extensions)
        return { html, hash: html ? bodyHash(html) : '', lineage: epochIn(doc) || '', open }
    } finally {
        await connection.disconnect()
    }
}

// stateToHTML is a saved state written as a body, or '' when it holds nothing.
export function stateToHTML(stateB64, extensions) {
    const ydoc = new Y.Doc()
    Y.applyUpdate(ydoc, Buffer.from(stateB64, 'base64'))
    const json = TiptapTransformer.fromYdoc(ydoc, FIELD)
    if (!json || !Array.isArray(json.content) || json.content.length === 0) return ''
    return renderDocHTML(json, extensions).html
}

// render writes a saved Yjs state as HTML with this service's schema, for the
// API's repair command (business/Doc docRepair.go), and says whether the doc is
// open here (the repair leaves an open doc alone: the copy in memory is saved
// over anything written underneath it). With no state it only answers `open`.
// Reads and writes nothing.
export async function render({ docName, body, hocuspocus, deps }) {
    const open = !!hocuspocus?.documents?.has(docName.toLowerCase())
    const state = typeof body?.state === 'string' ? body.state : ''
    if (!state) return { html: '', open }
    try {
        return { html: stateToHTML(state, deps.extensions), open }
    } catch (e) {
        throw refuse(422, `the state could not be written: ${e?.message || e}`)
    }
}
