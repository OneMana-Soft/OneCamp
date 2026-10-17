// The collaboration service: real Redis, the real API, and the process around
// them. Every hook lives in collabServer.js, where the tests run it.

import { Redis as RedisExtension } from '@hocuspocus/extension-redis'
import { Redis } from 'ioredis'
import axios from 'axios'
import jwt from 'jsonwebtoken'
import dotenv from 'dotenv'
import { createCollabServer } from './collabServer.js'
import { guardRedisExtension } from './guardRedis.js'
import { createLiveServer } from './live/server.js'

dotenv.config()

console.log('[Collab] Service starting...');

// ---------------------------------------------------------------------------
// Redis: horizontal scaling (the Hocuspocus extension), the crash-recovery
// buffer and contributor attribution (buffer.js, collabServer.js).
// ---------------------------------------------------------------------------
const redisConfig = {
    host: process.env.REDIS_HOST || 'localhost',
    port: process.env.REDIS_PORT ? parseInt(process.env.REDIS_PORT) : 6379,
    password: process.env.REDIS_PASSWORD,
    retryStrategy(times) {
        const delay = Math.min(times * 50, 2000);
        return delay;
    },
};

console.log(`[Collab] Connecting to Redis at ${redisConfig.host}:${redisConfig.port}...`);
const redisClient = new Redis(redisConfig);

redisClient.on('connect', () => console.log('[Collab] Redis connected successfully'));
redisClient.on('error', (err) => console.error('[Collab] Redis error:', err.message));

// Configuration for the Go Backend API
const GO_BACKEND_URL = process.env.GO_BACKEND_URL || 'http://localhost:3000'

// The secret this service presents to the API. There is no fallback: a default
// written here would be public, and an install left on it would let anyone who
// read this file into every document. Unset means the call is refused upstream,
// which is the visible failure we want.
function internalSecret() {
    const s = (process.env.INTERNAL_SECRET || '').trim()
    if (!s) console.error('INTERNAL_SECRET is not set; the API will refuse document and board loads until it is')
    return s
}

// Internal API calls retry three times (2, 4, 8 s) when the API may answer
// differently next time: no answer, a 5xx, 408 or 429. A 4xx is an answer
// (not found, refused, not allowed), so it is returned at once.
const api = axios.create({
    timeout: 15000,
})

api.interceptors.response.use(
    (response) => response,
    async (error) => {
        const { config } = error
        const status = error?.response?.status
        const final = typeof status === 'number' && status >= 400 && status < 500 && status !== 408 && status !== 429
        if (!config || final || config.__retryCount >= 3) {
            return Promise.reject(error)
        }
        config.__retryCount = config.__retryCount || 0
        config.__retryCount += 1
        const delay = Math.pow(2, config.__retryCount) * 1000
        await new Promise((resolve) => setTimeout(resolve, delay))
        return api(config)
    }
)

const collab = createCollabServer({
    port: process.env.PORT ? parseInt(process.env.PORT) : 1234,
    redis: redisClient,
    api,
    backendUrl: GO_BACKEND_URL,
    getSecret: internalSecret,
    decodeToken: (token) => jwt.decode(token),
    // Its lock and publishing failures stay inside it (guardRedis.js).
    serverExtensions: [guardRedisExtension(new RedisExtension({ redis: redisClient }))],
})

// The Live Doc Writer (live/): server-side writes into open docs, on a port of
// its own that the compose files never route through Traefik, so only the API
// on the services' network reaches it, and only with INTERNAL_SECRET.
const LIVE_PORT = process.env.LIVE_PORT ? parseInt(process.env.LIVE_PORT) : 1235
const live = createLiveServer({ getSecret: internalSecret, hocuspocus: collab.server.hocuspocus, deps: collab.live })
live.on('error', (e) => console.error('[Live] Internal server error:', e?.message || e))
live.listen(LIVE_PORT, () => console.log(`[Live] Live Doc Writer listening on internal port ${LIVE_PORT}`))

collab.server.listen().then(() => {
    console.log(`[Collab] Server listening on port ${process.env.PORT || 1234}`);
    // Docs whose saves were failing when the service last stopped: their whole
    // state is in Redis, and they are saved again now.
    collab.recoverUnsaved().catch((e) => console.error('[Collab] Recovering unsaved docs failed:', e?.message || e))
})

// ─── Process lifecycle ───────────────────────────────────────────────────────
//
// exitAfterFlush waits for stdio to drain before exiting. process.exit() does
// not flush a stdout that is a pipe, which is what it is under Docker, so the
// last line before exiting was lost (observed: "Shutdown complete" never
// appeared in `docker logs` after a SIGTERM).
const exitAfterFlush = (code) => {
    let pending = 2
    const done = () => { if (--pending === 0) process.exit(code) }
    process.stdout.write('', done)
    process.stderr.write('', done)
    // Belt and braces: if a stream never drains, do not hang holding the container open.
    setTimeout(() => process.exit(code), 1000).unref()
}

// AN UNHANDLED REJECTION IS LOGGED, AND THE SERVICE CARRIES ON. Until 11 Oct
// 2026 it exited here, and that turned one failed save into a crash loop: the
// save's rejection escaped the Hocuspocus debouncer, the service exited, every
// doc and board dropped, and on restart the buffered edit failed again. Every
// known source is now handled where it starts (a save never leaves its hook:
// collabServer.js, saveFailures.js; Redis failures stay in the extension:
// guardRedis.js), so a rejection reaching here is one nobody foresaw. Exiting
// for it would drop every open doc and board and could loop the same way; the
// edits are buffered in Redis either way, so staying up is the safer choice.
// The line says which service and what, so it is found and fixed.
process.on('unhandledRejection', (reason) => {
    console.error('[Collab] UNHANDLED promise rejection (logged; the service stays up):',
        reason instanceof Error ? reason.stack : reason)
})

// An uncaught exception is a synchronous throw nothing caught: the process may
// be half-way through changing a document, so it exits and Docker
// (`restart: unless-stopped`) starts a clean one. Unsaved edits are in Redis.
process.on('uncaughtException', (err) => {
    console.error('[Collab] FATAL uncaught exception — exiting so the container restarts:', err.stack || err)
    exitAfterFlush(1)
})

// SIGTERM, SIGINT, SIGQUIT: stop retrying, let go of pinned docs, save and
// unload every doc (collab.stop), then exit. Owned here (stopOnSignals is off
// in collabServer.js), because Hocuspocus's own handler waits for every doc to
// unload, and a doc pinned while its saves fail never would. Docker sends
// SIGKILL after 10 s, so this gives up after 8; anything unsaved is in Redis
// for the next start.
let stopping = false
const stop = async (signal) => {
    if (stopping) return
    stopping = true
    console.log(`[Collab] ${signal}: saving open docs and stopping`)
    setTimeout(() => {
        console.error('[Collab] Shutdown took too long; exiting (unsaved edits are kept in Redis)')
        exitAfterFlush(0)
    }, 8000).unref()
    try {
        // No more server-side writes while the docs are saved and unloaded.
        live.close()
        await collab.stop()
        console.log('[Collab] Shutdown complete')
    } catch (e) {
        console.error('[Collab] Shutdown failed:', e?.message || e)
    }
    exitAfterFlush(0)
}
for (const signal of ['SIGTERM', 'SIGINT', 'SIGQUIT']) process.on(signal, () => stop(signal))
