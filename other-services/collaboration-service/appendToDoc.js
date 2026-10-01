// Appending to a document from the server, as a real collaborative edit.
//
// WHY THIS EXISTS. An agent asked to "add the rollback steps to the Launch sync
// notes" could not: docs are Yjs documents held here, and doc_body in Postgres is
// only where this service persists a snapshot. Writing doc_body directly was
// built once and thrown away, because whoever has the doc open holds the
// authoritative copy and the next save silently overwrites the edit.
//
// This applies the change where the document lives. A Hocuspocus direct
// connection loads the document through the same onLoadDocument hook as any
// editor (or joins the copy already in memory), the new blocks are inserted in
// one Yjs transaction that every open editor receives live, and the normal
// onStoreDocument path saves it. Nothing races, because there is one copy.
//
// Permission is decided by the Go backend before it calls here (the agent's
// principal must be allowed to edit the doc). This endpoint trusts only the
// shared INTERNAL_SECRET, and refuses everything when that secret is not set:
// the service is reachable from browsers, so a default secret would let anyone
// on the internet write into any document.

import { timingSafeEqual } from 'node:crypto'
import * as Y from 'yjs'
import { TiptapTransformer } from '@hocuspocus/transformer'
import { generateJSON } from '@tiptap/html'

export const APPEND_PATH = /^\/internal\/docs\/([0-9a-fA-F-]{36})\/append$/
export const MAX_APPEND_BYTES = 256 * 1024

// internalSecretOK compares the presented secret with the configured one in
// constant time. An unset or blank configured secret never matches.
export function internalSecretOK(presented, configured) {
    if (typeof configured !== 'string' || configured.trim() === '') return false
    if (typeof presented !== 'string') return false
    const a = Buffer.from(presented)
    const b = Buffer.from(configured)
    return a.length === b.length && timingSafeEqual(a, b)
}

// htmlToYNodes turns HTML into top-level Yjs nodes ready to insert into another
// document's 'default' fragment. The nodes are built in a scratch document with
// the same extensions the editor uses, then cloned: a Yjs type belongs to one
// document, and a clone is the unattached copy another document can take.
export function htmlToYNodes(html, extensions) {
    const json = generateJSON(html, extensions)
    const scratch = TiptapTransformer.toYdoc(json, 'default', extensions)
    return scratch.getXmlFragment('default').toArray().map((node) => node.clone())
}

// appendNodes adds nodes to the end of a document's body in one transaction.
export function appendNodes(ydoc, nodes) {
    const body = ydoc.getXmlFragment('default')
    ydoc.transact(() => {
        body.insert(body.length, nodes)
    })
}

function send(response, status, body) {
    response.writeHead(status, { 'Content-Type': 'application/json' })
    response.end(JSON.stringify(body))
}

function readBody(request, limit) {
    return new Promise((resolve, reject) => {
        let size = 0
        const chunks = []
        request.on('data', (chunk) => {
            size += chunk.length
            if (size > limit) {
                reject(Object.assign(new Error('too large'), { status: 413 }))
                request.destroy()
                return
            }
            chunks.push(chunk)
        })
        request.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')))
        request.on('error', reject)
    })
}

// createAppendHandler returns an onRequest hook. It answers only its own path,
// and lets every other request through to Hocuspocus's default handling.
export function createAppendHandler({ getSecret, extensions, recordContributor }) {
    return async ({ request, response, instance }) => {
        const url = new URL(request.url || '/', 'http://collab.internal')
        const match = url.pathname.match(APPEND_PATH)
        if (!match) return

        try {
            if (request.method !== 'POST') {
                send(response, 405, { msg: 'POST only' })
            } else if (typeof getSecret() !== 'string' || getSecret().trim() === '') {
                send(response, 503, { msg: 'INTERNAL_SECRET is not set, so server-side document edits are off' })
            } else if (!internalSecretOK(request.headers['x-internal-secret'], getSecret())) {
                send(response, 401, { msg: 'not authorised' })
            } else {
                const docId = match[1].toLowerCase()
                let payload
                try {
                    payload = JSON.parse(await readBody(request, MAX_APPEND_BYTES))
                } catch (e) {
                    send(response, e.status || 400, { msg: e.status === 413 ? 'content too large' : 'body must be JSON' })
                    throw null
                }
                const html = typeof payload?.html === 'string' ? payload.html.trim() : ''
                const editor = typeof payload?.editor_uuid === 'string' ? payload.editor_uuid.trim() : ''
                if (!html) {
                    send(response, 400, { msg: 'html is required' })
                    throw null
                }
                const nodes = htmlToYNodes(html, extensions)
                if (nodes.length === 0) {
                    send(response, 400, { msg: 'nothing to add' })
                    throw null
                }
                const connection = await instance.openDirectConnection(docId, { user: { id: editor || 'unknown' } })
                try {
                    if (editor && recordContributor) await recordContributor(docId, editor)
                    await connection.transact((doc) => appendNodes(doc, nodes))
                } finally {
                    await connection.disconnect()
                }
                send(response, 200, { data: { appended_blocks: nodes.length } })
            }
        } catch (err) {
            if (err !== null) {
                console.error('[Collab] append failed:', err?.message || err)
                if (!response.headersSent) send(response, 500, { msg: 'append failed' })
            }
        }
        // This request is answered; stop Hocuspocus's default "Welcome" reply.
        throw null
    }
}
