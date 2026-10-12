// Test support for collabServer.test.js: an in-memory Redis, an API that
// behaves like the Go endpoints the service calls, and a minimal Hocuspocus
// client over a real websocket (the protocol a browser's provider speaks).
// Not a test file itself (node --test runs *.test.js only).

import { createRequire } from 'node:module'
import * as Y from 'yjs'
import { bodyHash } from './docState.js'
import { createCollabServer } from './collabServer.js'

const require = createRequire(import.meta.url)
// ws comes with Hocuspocus (no direct dependency needed for tests).
const WebSocket = createRequire(require.resolve('@hocuspocus/server'))('ws')

// ---------------------------------------------------------------------------
// Redis
// ---------------------------------------------------------------------------
export class FakeRedis {
    constructor() {
        this.data = new Map()
        this.down = false
    }
    check() {
        if (this.down) throw new Error('Connection is closed.')
    }
    list(k) {
        if (!this.data.has(k)) this.data.set(k, [])
        return this.data.get(k)
    }
    async rpush(k, ...v) { this.check(); this.list(k).push(...v); return this.list(k).length }
    async lrange(k, a, b) { this.check(); const l = this.data.get(k) || []; return l.slice(a, b === -1 ? undefined : b + 1) }
    async llen(k) { this.check(); return (this.data.get(k) || []).length }
    async ltrim(k, a, b) {
        this.check()
        const l = this.data.get(k) || []
        const n = l.length
        const from = a < 0 ? Math.max(n + a, 0) : a
        const to = b < 0 ? n + b : b
        this.data.set(k, l.slice(from, to + 1))
        return 'OK'
    }
    async lrem(k, count, v) {
        this.check()
        const l = this.data.get(k) || []
        const i = l.indexOf(v)
        if (i >= 0) l.splice(i, 1)
        return i >= 0 ? 1 : 0
    }
    async del(...ks) { this.check(); let n = 0; for (const k of ks) n += this.data.delete(k) ? 1 : 0; return n }
    async get(k) { this.check(); const v = this.data.get(k); return typeof v === 'string' ? v : null }
    async set(k, v) { this.check(); this.data.set(k, v); return 'OK' }
    async exists(k) { this.check(); return this.data.has(k) ? 1 : 0 }
    async expire() { this.check(); return 1 }
    async sadd(k, ...v) { this.check(); if (!this.data.has(k)) this.data.set(k, new Set()); for (const x of v) this.data.get(k).add(x); return 1 }
    async smembers(k) { this.check(); return [...(this.data.get(k) || [])] }
    async scan(cursor, _m, pattern) {
        this.check()
        const prefix = pattern.replace(/\*$/, '')
        return ['0', [...this.data.keys()].filter((k) => k.startsWith(prefix))]
    }
}

// ---------------------------------------------------------------------------
// The Go API, as the collaboration service sees it
// ---------------------------------------------------------------------------
const httpError = (status, msg) => Object.assign(new Error(`Request failed with status code ${status}`), { response: { status, data: { msg } } })

export class FakeBackend {
    constructor() {
        this.docs = new Map() // id -> { doc_body, doc_yjs_state, doc_yjs_body_hash }
        this.boards = new Map() // id -> { board_state }
        this.getStatus = null // e.g. 503 to fail loads
        this.saveStatus = null // e.g. 503 or 422 to fail saves
        this.authStatus = null // e.g. 410 to refuse joining a deleted doc
        this.dropStateOnly = false // ignore keepBody writes (no saved state)
        this.saves = []
        this.saveAttempts = 0 // every save asked for, refused or not
        this.api = {
            get: async (url) => this.get(url),
            post: async (url, body) => this.post(url, body),
        }
    }
    doc(id, body, extra = {}) {
        this.docs.set(id, { doc_body: body, ...extra })
    }
    async get(url) {
        const path = new URL(url).pathname
        if (this.getStatus) throw httpError(this.getStatus, 'unavailable')
        let m = path.match(/^\/docColab\/getDoc\/(.+)$/)
        if (m) {
            const d = this.docs.get(m[1])
            if (!d) throw httpError(404, "There's no such doc.")
            return { status: 200, data: { data: { ...d } } }
        }
        m = path.match(/^\/boardColab\/getBoard\/(.+)$/)
        if (m) {
            const b = this.boards.get(m[1])
            if (!b) throw httpError(404, "There's no such board.")
            return { status: 200, data: { data: { ...b } } }
        }
        throw httpError(404, `no route ${path}`)
    }
    async post(url, body) {
        const path = new URL(url).pathname
        if (/\/(docColab|boardColab)\/(authorize|guestAuthorize)$/.test(path)) {
            if (this.authStatus) throw httpError(this.authStatus, 'refused')
            return { status: 200, data: { canEdit: true } }
        }
        if (path === '/docColab/updateDoc' || path === '/boardColab/updateBoard') this.saveAttempts++
        if (path === '/docColab/updateDoc') {
            if (this.saveStatus) throw httpError(this.saveStatus, 'refused')
            const d = this.docs.get(body.documentId)
            if (!d) throw httpError(404, "There's no such doc.")
            if (body.keepBody) {
                if (this.dropStateOnly) return { status: 200 }
                const hash = bodyHash(d.doc_body)
                if (body.expectBodyHash && body.expectBodyHash !== hash) return { status: 200 }
                Object.assign(d, { doc_yjs_state: body.yjsState, doc_yjs_body_hash: hash })
                this.saves.push({ id: body.documentId, keepBody: true })
                return { status: 200 }
            }
            const html = body.htmlContent || ''
            if (!html.trim() || html.trim() === '<p>Error generating HTML</p>') throw httpError(422, 'unsafe body')
            Object.assign(d, { doc_body: html, doc_yjs_state: body.yjsState || d.doc_yjs_state, doc_yjs_body_hash: body.yjsState ? bodyHash(html) : d.doc_yjs_body_hash })
            this.saves.push({ id: body.documentId, html })
            return { status: 200 }
        }
        if (path === '/boardColab/updateBoard') {
            if (this.saveStatus) throw httpError(this.saveStatus, 'refused')
            this.boards.set(body.documentId, { board_state: body.state })
            this.saves.push({ id: body.documentId, board: true })
            return { status: 200 }
        }
        throw httpError(404, `no route ${path}`)
    }
}

// ---------------------------------------------------------------------------
// A server on a free port
// ---------------------------------------------------------------------------
export const quietLog = () => {
    const lines = []
    const rec = (...a) => lines.push(a.map((x) => (x instanceof Error ? x.message : String(x))).join(' '))
    return { log: rec, warn: rec, error: rec, lines }
}

export async function startServer({ redis, backend, log = quietLog(), ...options } = {}) {
    const collab = createCollabServer({
        port: 0,
        quiet: true,
        redis,
        api: backend.api,
        backendUrl: 'http://api.test',
        getSecret: () => 'test-secret',
        decodeToken: (t) => { try { return JSON.parse(t) } catch { return null } },
        log,
        debounce: 20,
        maxDebounce: 200,
        retryDelays: [30, 60, 120],
        ...options,
    })
    await collab.server.listen()
    collab.url = `ws://127.0.0.1:${collab.server.address.port}`
    collab.logs = log
    return collab
}

// ---------------------------------------------------------------------------
// A client: what a browser's Hocuspocus provider sends and does
// ---------------------------------------------------------------------------
const writeVarUint = (out, n) => {
    while (n > 0x7f) {
        out.push((n & 0x7f) | 0x80)
        n = Math.floor(n / 128)
    }
    out.push(n)
}
const writeVarString = (out, s) => {
    const b = Buffer.from(s, 'utf8')
    writeVarUint(out, b.length)
    out.push(...b)
}
const writeVarBytes = (out, b) => {
    writeVarUint(out, b.length)
    for (const x of b) out.push(x)
}
class Reader {
    constructor(buf) { this.b = buf; this.i = 0 }
    uint() { let n = 0; let m = 1; for (;;) { const x = this.b[this.i++]; n += (x & 0x7f) * m; if (x < 0x80) return n; m *= 128 } }
    bytes() { const n = this.uint(); const v = this.b.subarray(this.i, this.i + n); this.i += n; return v }
    string() { return Buffer.from(this.bytes()).toString('utf8') }
}

export class TestClient {
    constructor({ url, name, token = '{"sub":"user-1"}', schema, doc = new Y.Doc() }) {
        this.url = url
        this.name = name
        this.token = token
        this.schema = schema
        this.doc = doc
        this.events = []
        this.stateless = []
        this.statuses = []
        this.onUpdate = (update, origin) => {
            if (origin === this) return
            this.send(0, 2, update)
        }
        this.doc.on('update', this.onUpdate)
    }
    msg(type, sub, payload) {
        const out = []
        writeVarString(out, this.name)
        writeVarUint(out, type)
        if (type === 2) {
            writeVarUint(out, 0)
            writeVarString(out, payload)
        } else {
            writeVarUint(out, sub)
            writeVarBytes(out, payload)
        }
        return Buffer.from(out)
    }
    send(type, sub, payload) {
        if (this.ws?.readyState === WebSocket.OPEN && this.authenticated) this.ws.send(this.msg(type, sub, payload))
    }
    connect() {
        this.authenticated = false
        this.denied = null
        this.synced = false
        const url = this.schema ? `${this.url}/?schema=${this.schema}` : this.url
        this.ws = new WebSocket(url)
        this.ws.binaryType = 'nodebuffer'
        return new Promise((resolve) => {
            this.ws.on('open', () => this.ws.send(this.msg(2, 0, this.token)))
            this.ws.on('message', (data) => this.handle(Buffer.from(data), resolve))
            this.ws.on('close', () => resolve('closed'))
        })
    }
    handle(buf, resolve) {
        const r = new Reader(buf)
        r.string() // name
        const type = r.uint()
        if (type === 2) {
            const auth = r.uint()
            if (auth === 2) {
                this.scope = r.string()
                this.authenticated = true
                // as the provider: send sync step 1
                this.ws.send(this.msg(0, 0, Y.encodeStateVector(this.doc)))
            } else if (auth === 1) {
                this.denied = r.string()
                resolve('denied')
            }
        } else if (type === 0 || type === 4) {
            const sync = r.uint()
            const payload = r.bytes()
            if (sync === 0) {
                // the server's state vector: send what it lacks (sync step 2)
                this.ws.send(this.msg(0, 1, Y.encodeStateAsUpdate(this.doc, payload)))
            } else {
                Y.applyUpdate(this.doc, payload, this)
                if (sync === 1 && !this.synced) {
                    this.synced = true
                    resolve('synced')
                }
            }
        } else if (type === 5) {
            this.stateless.push(r.string())
        } else if (type === 8) {
            this.statuses.push(r.uint() === 1)
        }
    }
    async close() {
        if (!this.ws) return
        const ws = this.ws
        this.ws = null
        await new Promise((resolve) => {
            ws.once('close', resolve)
            ws.close()
        })
    }
    destroy() {
        this.doc.off('update', this.onUpdate)
    }
}

// Wait until a condition holds (polling), or fail after `ms`.
export async function until(cond, ms = 3000, what = 'condition') {
    const t0 = Date.now()
    for (;;) {
        if (await cond()) return
        if (Date.now() - t0 > ms) throw new Error(`timed out waiting for ${what}`)
        await new Promise((r) => setTimeout(r, 10))
    }
}
export const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
