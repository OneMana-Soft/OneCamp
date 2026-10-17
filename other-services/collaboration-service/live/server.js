// The internal server for the Live Doc Writer's ops (live/routes.js).
//
// It listens on a port of its own that is never published through Traefik, so
// only the API can reach it, and it answers only callers that present the shared
// INTERNAL_SECRET: POST /live/docs/<doc uuid>/<op> with a JSON body. Unlike the
// public port, nothing here is reachable from a browser.
//
// index.js starts it on LIVE_PORT (1235 unless set). The compose files route only
// the public port (1234) through Traefik, so this one is reachable on the
// services' own network alone.

import http from 'node:http'
import { internalSecretOK } from './secret.js'
import { LIVE_ROUTES } from './routes.js'

export const LIVE_PATH = /^\/live\/docs\/([0-9a-fA-F-]{36})\/([A-Za-z]+)$/
// A whole doc's HTML (replaceAll, seed) or a saved state (render).
export const MAX_LIVE_BYTES = 16 * 1024 * 1024

function send(response, status, body, headers = {}) {
    response.writeHead(status, { 'Content-Type': 'application/json', ...headers })
    response.end(JSON.stringify(body))
}

// readBody reads at most max bytes of a request's body and answers null past
// that. It stops keeping what arrives, but lets the rest drain, so the answer
// still reaches the caller: destroying the request would close the socket the
// answer goes out on.
function readBody(request, max) {
    return new Promise((resolve, reject) => {
        const chunks = []
        let size = 0
        let over = false
        request.on('data', (chunk) => {
            if (over) return
            size += chunk.length
            if (size > max) {
                over = true
                resolve(null)
                return
            }
            chunks.push(chunk)
        })
        request.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')))
        request.on('error', reject)
    })
}

// createLiveHandler answers the ops in routes. getSecret returns the configured
// INTERNAL_SECRET; with none set, every call is refused. deps is what the ops
// share (collabServer.js `live`: the schema, the contributors, the persistence
// record).
export function createLiveHandler({ getSecret, routes = LIVE_ROUTES, hocuspocus = null, deps = {} }) {
    return async (request, response) => {
        if (!internalSecretOK(request.headers['x-internal-secret'], getSecret())) {
            send(response, 401, { msg: 'unauthorised' })
            return
        }
        const match = LIVE_PATH.exec((request.url || '').split('?')[0])
        const op = match && routes.get(match[2])
        if (!op) {
            send(response, 404, { msg: 'no such op' })
            return
        }
        if (request.method !== 'POST') {
            send(response, 405, { msg: 'POST only' })
            return
        }
        const raw = await readBody(request, MAX_LIVE_BYTES)
        if (raw === null) {
            send(response, 413, { msg: 'body too large' }, { Connection: 'close' })
            return
        }
        let body
        try {
            body = raw ? JSON.parse(raw) : {}
        } catch {
            send(response, 400, { msg: 'body is not JSON' })
            return
        }
        try {
            const data = await op({ docName: match[1].toLowerCase(), body, hocuspocus, deps })
            send(response, 200, { msg: 'ok', data: data ?? null })
        } catch (err) {
            const status = Number.isInteger(err?.status) && err.status >= 400 && err.status < 600 ? err.status : 500
            if (status >= 500) console.error(`[Live] ${match[2]} failed for ${match[1]}:`, err)
            send(response, status, { msg: status >= 500 ? 'the op failed' : String(err?.message || 'refused') })
        }
    }
}

// createLiveServer builds the internal server, unstarted: the caller listens on
// the internal port.
export function createLiveServer(options) {
    return http.createServer(createLiveHandler(options))
}
