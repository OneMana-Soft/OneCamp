// Writing a doc's saved Yjs state as HTML, for the backend's repair command
// (business/Doc/docRepair.go, `go-one-camp repairdocs`).
//
// POST /internal/docs/<id>/render {state: base64} answers {html, open}: the
// state written as a body with the schema this service saves with, and whether
// the doc is open here now (the repair leaves an open doc alone, because the
// copy in memory is saved over anything written underneath it). With no state
// it only answers `open`. Reads nothing and writes nothing; trusts only the
// shared INTERNAL_SECRET, like the append route.

import * as Y from 'yjs'
import { TiptapTransformer } from '@hocuspocus/transformer'
import { internalSecretOK, readBody, send } from './appendToDoc.js'
import { renderDocHTML } from './preserved.js'

export const RENDER_PATH = /^\/internal\/docs\/([0-9a-fA-F-]{36})\/render$/
export const MAX_RENDER_BYTES = 64 * 1024 * 1024

// stateToHTML is a saved state written as a body, or '' when it holds nothing.
export function stateToHTML(stateB64, extensions) {
    const ydoc = new Y.Doc()
    Y.applyUpdate(ydoc, Buffer.from(stateB64, 'base64'))
    const json = TiptapTransformer.fromYdoc(ydoc, 'default')
    if (!json || !Array.isArray(json.content) || json.content.length === 0) return ''
    return renderDocHTML(json, extensions).html
}

export function createRenderHandler({ getSecret, extensions }) {
    return async ({ request, response, instance }) => {
        const url = new URL(request.url || '/', 'http://collab.internal')
        const match = url.pathname.match(RENDER_PATH)
        if (!match) return

        try {
            if (request.method !== 'POST') {
                send(response, 405, { msg: 'POST only' })
            } else if (typeof getSecret() !== 'string' || getSecret().trim() === '') {
                send(response, 503, { msg: 'INTERNAL_SECRET is not set' })
            } else if (!internalSecretOK(request.headers['x-internal-secret'], getSecret())) {
                send(response, 401, { msg: 'not authorised' })
            } else {
                const docId = match[1].toLowerCase()
                let payload
                try {
                    payload = JSON.parse(await readBody(request, MAX_RENDER_BYTES))
                } catch (e) {
                    send(response, e.status || 400, { msg: e.status === 413 ? 'state too large' : 'body must be JSON' })
                    throw null
                }
                const open = !!instance?.documents?.has(docId)
                const state = typeof payload?.state === 'string' ? payload.state : ''
                let html = ''
                if (state) {
                    try {
                        html = stateToHTML(state, extensions)
                    } catch (e) {
                        send(response, 422, { msg: `the state could not be written: ${e?.message || e}` })
                        throw null
                    }
                }
                send(response, 200, { data: { html, open } })
            }
        } catch (err) {
            if (err !== null) {
                console.error('[Collab] render failed:', err?.message || err)
                if (!response.headersSent) send(response, 500, { msg: 'render failed' })
            }
        }
        // Answered; stop Hocuspocus's default reply.
        throw null
    }
}
