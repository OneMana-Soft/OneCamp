
import { Server } from '@hocuspocus/server'
import { Redis as RedisExtension } from '@hocuspocus/extension-redis'
import { Redis } from 'ioredis'
import axios from 'axios'
import jwt from 'jsonwebtoken'
import dotenv from 'dotenv'
import * as Y from 'yjs'

dotenv.config()

console.log('[Collab] Service starting...');

// ---------------------------------------------------------------------------
// Document namespacing
// ---------------------------------------------------------------------------
// Docs connect with the bare doc uuid as the document name. Boards connect with
// a "board:" prefix so the single Hocuspocus server can route both kinds of
// documents to the right backend endpoints. Boards use a raw Yjs document
// (Excalidraw elements), not the Tiptap transform pipeline used for docs.
const BOARD_PREFIX = 'board:'

const parseDocumentName = (documentName) => {
    if (typeof documentName === 'string' && documentName.startsWith(BOARD_PREFIX)) {
        return { kind: 'board', id: documentName.slice(BOARD_PREFIX.length) }
    }
    return { kind: 'doc', id: documentName }
}

// ---------------------------------------------------------------------------
// Per-document contributor tracking (version-history attribution)
// ---------------------------------------------------------------------------
// A debounced save can aggregate edits from several people, so a version's
// "edited by" is a SET of users, not one. We accumulate the distinct editor
// user uuids that contributed since the last persist in a Redis set (keyed by
// document name) so attribution is correct even when editors are spread across
// horizontally-scaled collab nodes, and is naturally deduplicated. The set is
// read-and-cleared on each persist and attached to the snapshot; a TTL expires
// stale sets so a document that stops being edited cannot leak keys.
const CONTRIBUTORS_TTL_SECONDS = 60 * 60
const contributorsKey = (documentName) => `collab:contributors:${documentName}`

// ---------------------------------------------------------------------------
// 1. Configure Redis for horizontal scaling
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

// readAndClearContributors returns the distinct editor uuids accumulated since
// the last persist and clears the set. Best-effort: any Redis error yields an
// empty list so persistence is never blocked by attribution.
async function readAndClearContributors(documentName) {
    try {
        const key = contributorsKey(documentName)
        const members = await redisClient.smembers(key)
        if (members && members.length > 0) {
            await redisClient.del(key)
        }
        return Array.isArray(members) ? members.filter((m) => m && m !== 'unknown') : []
    } catch (e) {
        console.error(`[Collab] Failed to read contributors for ${documentName}:`, e.message)
        return []
    }
}

// ---------------------------------------------------------------------------
// Crash-recovery update buffer
// ---------------------------------------------------------------------------
// Saves are debounced (5s, max 30s). If this server crashes between saves, the
// edits held only in memory would be lost. To close that window we append every
// incremental Yjs update to a per-document Redis list as it happens. On load we
// replay any buffered updates on top of the last persisted snapshot; on a
// successful save we trim the updates the snapshot now includes. A TTL bounds
// memory for abandoned documents. All operations are best-effort: a Redis blip
// never blocks editing or persistence.
const UPDATES_TTL_SECONDS = 60 * 60
const updatesKey = (documentName) => `collab:updates:${documentName}`

async function bufferUpdate(documentName, update) {
    if (!update || !update.length) return
    try {
        const key = updatesKey(documentName)
        await redisClient.rpush(key, Buffer.from(update).toString('base64'))
        await redisClient.expire(key, UPDATES_TTL_SECONDS)
    } catch (e) {
        // best-effort
    }
}

async function bufferedUpdateCount(documentName) {
    try {
        return await redisClient.llen(updatesKey(documentName))
    } catch (e) {
        return 0
    }
}

// trimPersistedUpdates drops the first `n` buffered updates (those a just-saved
// snapshot already includes), keeping any that arrived during the save.
async function trimPersistedUpdates(documentName, n) {
    if (!n || n <= 0) return
    try {
        await redisClient.ltrim(updatesKey(documentName), n, -1)
    } catch (e) {
        // best-effort
    }
}

// applyBufferedUpdates replays the buffered (unsaved) updates onto a base doc.
// Yjs updates are idempotent, so replaying is safe even if some overlap the
// snapshot. Returns the number applied.
async function applyBufferedUpdates(documentName, ydoc) {
    let items = []
    try {
        items = await redisClient.lrange(updatesKey(documentName), 0, -1)
    } catch (e) {
        return 0
    }
    if (!Array.isArray(items) || items.length === 0) return 0
    for (const b64 of items) {
        try {
            Y.applyUpdate(ydoc, Buffer.from(b64, 'base64'))
        } catch (e) {
            // skip a corrupt buffered update
        }
    }
    console.log(`[Collab] Replayed ${items.length} buffered update(s) for ${documentName}`)
    return items.length
}

// Compaction: deleted Excalidraw elements stay in the Yjs map as isDeleted
// tombstones. Without pruning, a heavily-edited board grows forever (slower
// loads, bigger payloads). We drop tombstones older than this window (well
// beyond the undo horizon) on the server during persist, so the saved + synced
// document stays bounded to live content.
const TOMBSTONE_PRUNE_AGE_MS = 10 * 60 * 1000

import { TiptapTransformer } from '@hocuspocus/transformer'
import { generateHTML, generateJSON } from '@tiptap/html'
import { COLLAB_EXTENSIONS } from './extensions.js'
import { createAppendHandler } from './appendToDoc.js'

// The secret this service presents to the API. There is no fallback: a default
// written here would be public, and an install left on it would let anyone who
// read this file into every document. Unset means the call is refused upstream,
// which is the visible failure we want.
function internalSecret() {
    const s = (process.env.INTERNAL_SECRET || '').trim()
    if (!s) console.error('INTERNAL_SECRET is not set; the API will refuse document and board loads until it is')
    return s
}

// Helper to extract text from Tiptap JSON
const getText = (node) => {
    let text = ''
    if (node.type === 'text') {
        text += node.text
    }
    if (node.content) {
        node.content.forEach(child => {
            text += getText(child) + ' '
        })
    }
    return text.trim()
}

// Axios instance with retry logic for internal API calls
const api = axios.create({
    timeout: 15000,
})

api.interceptors.response.use(
    (response) => response,
    async (error) => {
        const { config } = error
        if (!config || config.__retryCount >= 3) {
            return Promise.reject(error)
        }
        config.__retryCount = config.__retryCount || 0
        config.__retryCount += 1
        const delay = Math.pow(2, config.__retryCount) * 1000
        await new Promise((resolve) => setTimeout(resolve, delay))
        return api(config)
    }
)

const server = new Server({
    port: process.env.PORT ? parseInt(process.env.PORT) : 1234,

    // -----------------------------------------------------------------------
    // PRODUCTION: Periodic auto-save even while clients are connected.
    // Without this, data is only saved when the LAST client disconnects.
    // Server restart = total data loss for active documents.
    // -----------------------------------------------------------------------
    debounce: 5000,      // Wait 5s after last change before saving
    maxDebounce: 30000,  // Force save every 30s regardless of activity

    extensions: [
        new RedisExtension({
            redis: redisClient,
        }),
    ],

    // Server-side appends from the Go backend (agents writing into a doc).
    // See appendToDoc.js; answers only POST /internal/docs/<id>/append.
    onRequest: createAppendHandler({
        getSecret: () => process.env.INTERNAL_SECRET,
        extensions: COLLAB_EXTENSIONS,
        recordContributor: async (documentName, userId) => {
            const key = contributorsKey(documentName)
            await redisClient.sadd(key, userId)
            await redisClient.expire(key, CONTRIBUTORS_TTL_SECONDS)
        },
    }),

    // 2. Authentication Hook
    async onAuthenticate(data) {
        const { token } = data;

        if (!token) {
            throw new Error('Authentication token required')
        }

        const { kind, id } = parseDocumentName(data.documentName)

        try {
            const decoded = jwt.decode(token)

            // Guest (Phase 2): a scoped, READ-ONLY external grant. A guest token
            // carries a `guest` marker; route it to the separate public guest
            // authorize endpoint, which verifies the guest JWT and re-validates
            // the grant (so revocation/expiry take effect on every reconnect).
            // Guests ALWAYS join read-only — Hocuspocus rejects their writes.
            if (decoded && decoded.guest === true) {
                const guestAuthUrl = kind === 'board'
                    ? `${GO_BACKEND_URL}/boardColab/guestAuthorize`
                    : `${GO_BACKEND_URL}/docColab/guestAuthorize`
                const guestAuthBody = kind === 'board' ? { board_uuid: id } : { doc_uuid: id }

                const gResp = await api.post(guestAuthUrl, guestAuthBody, {
                    headers: { Authorization: `Bearer ${token}` }
                })
                if (gResp.status !== 200) {
                    throw new Error('Unauthorized')
                }
                if (data.connection) {
                    data.connection.readOnly = true
                }
                console.log(`[Collab] Guest joined ${kind} ${id} read-only`)
                return {
                    user: {
                        id: decoded.sub || 'guest',
                        name: decoded.name || 'Guest',
                    },
                }
            }

            console.log(`[Collab] Authorizing user ${decoded?.sub} for ${kind} ${id}...`);

            const authUrl = kind === 'board'
                ? `${GO_BACKEND_URL}/boardColab/authorize`
                : `${GO_BACKEND_URL}/docColab/authorize`
            const authBody = kind === 'board'
                ? { board_uuid: id }
                : { doc_uuid: id }

            const response = await api.post(
                authUrl,
                authBody,
                { headers: { Authorization: `Bearer ${token}` } }
            )

            console.log(`[Collab] Auth response for ${kind} ${id}: ${response.status}`);

            if (response.status !== 200) {
                throw new Error('Unauthorized')
            }

            // Server-enforced read-only: for boards the authorize endpoint
            // returns canEdit. Viewers join the live session (to see edits +
            // cursors) but Hocuspocus rejects their document writes. Docs use
            // edit-access auth upstream, so they are always editable here.
            if (kind === 'board' && response.data && response.data.canEdit === false) {
                if (data.connection) {
                    data.connection.readOnly = true
                }
                console.log(`[Collab] Board ${id} joined read-only for user ${decoded?.sub}`);
            }

            return {
                user: {
                    id: decoded?.sub || 'unknown',
                    name: decoded?.name || 'Anonymous',
                },
            }
        } catch (error) {
            console.error('[Collab] Auth failed:', error.message)
            throw new Error('Authentication failed')
        }
    },

    // 3. Load Document Hook
    async onLoadDocument(data) {
        const { kind, id } = parseDocumentName(data.documentName)

        if (kind === 'board') {
            const fetchUrl = `${GO_BACKEND_URL}/boardColab/getBoard/${id}`;
            try {
                const response = await api.get(fetchUrl, {
                    headers: { 'X-Internal-Secret': internalSecret() }
                })

                const boardState = response.data?.data?.board_state

                if (typeof boardState === 'string' && boardState.length > 0) {
                    console.log(`[Collab] Loading existing board state for ${id} (${boardState.length} b64 chars)`);
                    // board_state is the base64-encoded full Yjs document update.
                    Y.applyUpdate(data.document, Buffer.from(boardState, 'base64'))
                } else {
                    console.log(`[Collab] No existing board state for ${id}, starting empty board`);
                }
            } catch (error) {
                console.error(`[Collab] Failed to load board ${id}:`, error.message)
            }
            // Replay any edits buffered since the last persisted save (crash
            // recovery), so a server restart never loses recent work.
            await applyBufferedUpdates(data.documentName, data.document)
            return data.document
        }

        const fetchUrl = `${GO_BACKEND_URL}/docColab/getDoc/${id}`;

        try {
            const response = await api.get(fetchUrl, {
                headers: { 'X-Internal-Secret': internalSecret() }
            })

            const docBody = response.data?.data?.doc_body

            if (typeof docBody === 'string' && docBody.trim().length > 0) {
                console.log(`[Collab] Loading existing content for ${id} (${docBody.length} chars)`);
                const json = generateJSON(docBody, COLLAB_EXTENSIONS)
                const ydoc = TiptapTransformer.toYdoc(json, 'default', COLLAB_EXTENSIONS)
                await applyBufferedUpdates(data.documentName, ydoc)
                return ydoc
            } else {
                console.log(`[Collab] No existing content found for ${id}, creating empty doc`);
            }
        } catch (error) {
            console.error(`[Collab] Failed to load document ${id}:`, error.message)
        }

        // No persisted body: still replay any buffered (unsaved) edits so a
        // crash between saves cannot lose a freshly-created document. If there
        // is nothing buffered, return undefined to let Hocuspocus create a
        // fresh empty Yjs document.
        const recovered = new Y.Doc()
        const replayed = await applyBufferedUpdates(data.documentName, recovered)
        return replayed > 0 ? recovered : undefined
    },

    // 4. Persistence Hook
    async onStoreDocument(data) {
        const { kind, id } = parseDocumentName(data.documentName)

        // Number of buffered updates the about-to-be-saved snapshot will
        // include; trimmed from the crash-recovery buffer after a successful
        // save (captured before any doc mutation below).
        const persistedUpdateCount = await bufferedUpdateCount(data.documentName)

        if (kind === 'board') {
            // Compact the document: drop deleted-element tombstones older than
            // the prune window so the saved + synced board stays bounded to live
            // content and never bloats load times as it is edited over time.
            try {
                const elementsMap = data.document.getMap('elements')
                const now = Date.now()
                const stale = []
                elementsMap.forEach((val, key) => {
                    if (
                        val && val.isDeleted === true &&
                        typeof val.updated === 'number' &&
                        now - val.updated > TOMBSTONE_PRUNE_AGE_MS
                    ) {
                        stale.push(key)
                    }
                })
                if (stale.length > 0) {
                    data.document.transact(() => {
                        for (const k of stale) elementsMap.delete(k)
                    }, 'prune')
                    console.log(`[Collab] Pruned ${stale.length} deleted element(s) from board ${id}`)
                }
            } catch (e) {
                console.error(`[Collab] Tombstone prune failed for ${id}:`, e.message)
            }

            // Persist the entire Yjs document as a base64 update. This is canvas
            // data (Excalidraw elements), so there is no Tiptap/HTML transform.
            let stateB64 = ''
            try {
                stateB64 = Buffer.from(Y.encodeStateAsUpdate(data.document)).toString('base64')
            } catch (e) {
                console.error(`[Collab] Failed to encode board state for ${id}:`, e.message)
                return
            }

            // Skip persisting a brand-new empty board (no elements yet) to avoid
            // clobbering content on transient connections. The canvas stores
            // elements in a Y.Map keyed by element id (see BoardCanvas), so we
            // must read it as a map - reading it as an array would throw a Yjs
            // type-constructor error.
            const elements = data.document.getMap('elements')
            const elementCount = elements ? elements.size : 0
            if (elementCount === 0 && stateB64.length < 40) {
                console.log(`[Collab] Board ${id} is empty, skipping save`);
                return
            }

            const snippet = `${elementCount} elements`

            console.log(`[Collab] Saving board ${id} to backend...`);
            const contributors = await readAndClearContributors(data.documentName)
            try {
                const response = await api.post(`${GO_BACKEND_URL}/boardColab/updateBoard`, {
                    documentId: id,
                    state: stateB64,
                    snippet,
                    contributors,
                }, {
                    headers: {
                        'X-Internal-Secret': internalSecret()
                    }
                })
                console.log(`[Collab] Saved board ${id} successfully: ${response.status}`);
                // Snapshot now durable: drop the updates it covers.
                await trimPersistedUpdates(data.documentName, persistedUpdateCount)
            } catch (error) {
                console.error(`[Collab] Failed to save board ${id}:`, error.message)
                throw error
            }
            return
        }

        const json = TiptapTransformer.fromYdoc(data.document).default

        if (!json) {
            console.warn(`[Collab] Attempted to save empty/invalid document for ${id}`);
            return
        }

        // Defensive: check if document is actually empty
        const isEmptyDoc =
            !json.content ||
            json.content.length === 0 ||
            (json.content.length === 1 &&
                json.content[0].type === 'paragraph' &&
                (!json.content[0].content || json.content[0].content.length === 0))

        if (isEmptyDoc) {
            console.log(`[Collab] Document ${id} is empty, skipping save to avoid overwriting content`);
            return
        }

        let html = ''
        try {
            html = generateHTML(json, COLLAB_EXTENSIONS)
        } catch (e) {
            console.error('[Collab] HTML Generation failed:', e)
            html = '<p>Error generating HTML</p>'
        }

        console.log(`[Collab] Saving document ${id} to backend...`);
        const contributors = await readAndClearContributors(data.documentName)
        try {
            const response = await api.post(`${GO_BACKEND_URL}/docColab/updateDoc`, {
                documentId: id,
                content: json,
                textContent: getText(json),
                htmlContent: html,
                contributors,
            }, {
                headers: {
                    'X-Internal-Secret': internalSecret()
                }
            })
            console.log(`[Collab] Saved ${id} successfully: ${response.status}`);
            // Snapshot now durable: drop the updates it covers.
            await trimPersistedUpdates(data.documentName, persistedUpdateCount)
        } catch (error) {
            console.error(`[Collab] Failed to save document ${id}:`, error.message)
            // Throwing here will keep the document in memory and retry on next debounce
            throw error
        }
    },

    // 5. Connection lifecycle hooks for debugging
    async onChange(data) {
        // Append the incremental update to the crash-recovery buffer so edits
        // made between debounced saves survive a server crash.
        await bufferUpdate(data.documentName, data.update)

        // Accumulate the editing user so the next persist can attribute the
        // version to the full set of contributors (see CONTRIBUTORS_TTL above).
        try {
            const userId = data?.context?.user?.id
            if (!userId || userId === 'unknown') return
            const key = contributorsKey(data.documentName)
            await redisClient.sadd(key, userId)
            await redisClient.expire(key, CONTRIBUTORS_TTL_SECONDS)
        } catch (e) {
            // Non-fatal: attribution is best-effort.
        }
    },

    async onConnect(data) {
        console.log(`[Collab] Client connected to ${data.documentName} (total: ${data.clientsCount})`);
    },

    async onDisconnect(data) {
        console.log(`[Collab] Client disconnected from ${data.documentName} (remaining: ${data.clientsCount})`);
    },

    // WRONG HOOK, WRONG FIELD. This was `onDestroy(data)` logging
    // `Document ${data.documentName} destroyed from memory`, and it could never say anything true:
    // onDestroy is the SERVER-level hook and receives { instance } only, so documentName was always
    // undefined. Every shutdown printed "Document undefined destroyed from memory", twice, which
    // reads like a document bug during an event that has nothing to do with documents.
    //
    // afterUnloadDocument is the per-document one and receives { instance, documentName }.
    async afterUnloadDocument(data) {
        console.log(`[Collab] Document ${data.documentName} unloaded from memory`);
    },
    async onDestroy() {
        console.log('[Collab] Server destroyed — no longer accepting connections');
    },
})

server.listen()
console.log(`[Collab] Server listening on port ${process.env.PORT || 1234}`);

// ─── Process lifecycle ───────────────────────────────────────────────────────
//
// WHY THIS EXISTS. This process had no handler of any kind, and since Node 15 an unhandled promise
// rejection TERMINATES the process by default. Combined with the compose service having had no
// restart policy, one rejection anywhere in a hook meant the collaboration server stayed dead until
// somebody noticed — and the way you notice is every user reporting "Reconnecting to collaboration
// server..." on every document and board, forever, while the rest of the product works perfectly.
// The banner is honest: the client is fine and reconnecting correctly, there is simply nothing to
// reconnect to.
//
// Both fatal handlers LOG AND EXIT rather than swallowing. An unhandled rejection means some await
// chain is in a state nobody reasoned about; continuing risks serving documents from a process that
// has half-failed, and a corrupted Yjs update persisted to Postgres outlives the crash. Exiting
// non-zero hands the decision to Docker, which now has `restart: unless-stopped` and will bring back
// a clean process in a second. Loud, brief, and recoverable beats quiet and indefinite.
//
// The logs name the process explicitly, because `docker logs` on a crash-looping container otherwise
// shows a stack trace with no indication of which service produced it.

// exitAfterFlush waits for stdio to drain before exiting.
//
// NOT PEDANTRY — this was observed. process.exit() does not flush a stdout that is a PIPE, which is
// exactly what stdout is under Docker, so the last line written before exiting is simply lost. A
// SIGTERM test showed the process exiting 0 with "Shutdown complete" never appearing: the one line
// that distinguishes a clean shutdown from a killed one, missing from `docker logs`, on every deploy.
//
// Writing an empty string with a callback resolves once the stream has drained.
const exitAfterFlush = (code) => {
    let pending = 2
    const done = () => { if (--pending === 0) process.exit(code) }
    process.stdout.write('', done)
    process.stderr.write('', done)
    // Belt and braces: if a stream never drains, do not hang holding the container open.
    setTimeout(() => process.exit(code), 1000).unref()
}

process.on('unhandledRejection', (reason) => {
    console.error('[Collab] FATAL unhandled promise rejection — exiting so the container restarts:',
        reason instanceof Error ? reason.stack : reason)
    exitAfterFlush(1)
})

process.on('uncaughtException', (err) => {
    console.error('[Collab] FATAL uncaught exception — exiting so the container restarts:', err.stack || err)
    exitAfterFlush(1)
})

// NO SIGTERM HANDLER HERE, DELIBERATELY. Hocuspocus installs its own in listen() whenever
// `stopOnSignals` is set, which it is by default:
//
//     const signalHandler = async () => { await this.destroy(); process.exit(0) }
//     process.on('SIGINT', signalHandler)
//     process.on('SIGQUIT', signalHandler)
//     process.on('SIGTERM', signalHandler)
//
// So document flushing on shutdown was already handled. A second handler here does not add to that,
// it RACES it: two concurrent destroy() calls on the same server, and whichever finishes first calls
// process.exit and truncates the other.
//
// That is not a guess. A version of this file added its own handler, and a SIGTERM smoke test showed
// the documents tearing down and the process exiting 0 while the handler's own "Shutdown complete"
// never printed — the library's handler had won the race and exited first. Verifying destroy() in
// isolation showed it resolving normally, which is what pointed at the duplicate rather than at the
// promise.
//
// If shutdown behaviour ever needs to change, pass `stopOnSignals: false` to the Server config and
// own the whole thing here. Do not add a handler alongside theirs.
