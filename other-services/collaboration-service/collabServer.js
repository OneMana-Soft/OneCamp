// The collaboration server: every hook, with its dependencies passed in, so the
// tests run the same code against a fake Redis and a fake API (index.js wires
// the real ones and listens).
//
// Two rules hold everywhere here:
//   - A save never fails out of its hook. A failed save is logged, its edits
//     are kept and it is tried again (saveFailures.js). Before 11 Oct 2026 the
//     failure escaped as an unhandled rejection, index.js exited, and on
//     restart the buffered edit was replayed and failed again: a crash loop that
//     dropped every doc and board for as long as anyone edited that doc.
//   - A doc or board that could not be read is never opened empty. Served
//     empty, its first edit was saved over the stored copy. Loading throws
//     instead: Hocuspocus refuses the connection with the reason
//     "document-unavailable", and the web app says it couldn't open the doc,
//     with Retry, while showing the stored copy read-only.

import { Server } from '@hocuspocus/server'
import * as Y from 'yjs'
import { createAppendHandler } from './appendToDoc.js'
import { createUpdateBuffer } from './buffer.js'
import { bodyHash, encodeState, savedStateFor } from './docState.js'
import { COLLAB_EXTENSIONS } from './extensions.js'
import { createRenderHandler } from './renderDoc.js'
import { createSaveFailures } from './saveFailures.js'
import { EPOCH_CHANGED_MESSAGE, bringsSecondCopy, rebuildFromBody, syncUpdateOf } from './lineage.js'
import { SCHEMA_OUTDATED_MESSAGE, schemaVerdict } from './schemaGate.js'
import { persistBoard } from './storeBoard.js'
import { persistDoc } from './storeDoc.js'

// Docs connect with the bare doc uuid as the document name. Boards connect with
// a "board:" prefix so one server routes both to the right backend endpoints.
// Boards use a raw Yjs document (Excalidraw elements), not the Tiptap pipeline.
export const BOARD_PREFIX = 'board:'

export const parseDocumentName = (documentName) => {
    if (typeof documentName === 'string' && documentName.startsWith(BOARD_PREFIX)) {
        return { kind: 'board', id: documentName.slice(BOARD_PREFIX.length) }
    }
    return { kind: 'doc', id: documentName }
}

// The reason a refused load gives the client (Hocuspocus sends it as the
// permission-denied reason). The web app shows "Couldn't open this doc" with
// Retry for it (hooks/useCollaborationProvider.ts).
export const UNAVAILABLE = 'document-unavailable'

// A debounced save can aggregate edits from several people, so a version's
// "edited by" is a SET of users. The distinct editor uuids since the last save
// are kept in a Redis set (so it works across instances), read and cleared on
// each save, and expire if the doc stops being edited.
export const CONTRIBUTORS_TTL_SECONDS = 60 * 60
export const contributorsKey = (documentName) => `collab:contributors:${documentName}`

const refusal = (what, cause) => {
    const status = cause?.response?.status
    const err = new Error(`could not open ${what}: ${status ? `HTTP ${status}: ` : ''}${cause?.message || cause}`)
    err.reason = UNAVAILABLE
    err.response = cause?.response
    err.deterministic = !!cause?.deterministic
    return err
}

export function createCollabServer({
    redis,
    api,
    backendUrl,
    getSecret,
    decodeToken,
    extensions = COLLAB_EXTENSIONS,
    serverExtensions = [],
    log = console,
    debounce = 5000,
    maxDebounce = 30000,
    retryDelays,
    poisonAfter,
    timers,
    // The schema gate's view (schemaGate.js); tests pass their own.
    schema = {},
    ...serverOptions
}) {
    const buffer = createUpdateBuffer(redis, { log })
    const internalHeaders = () => ({ headers: { 'X-Internal-Secret': getSecret() } })

    async function readAndClearContributors(documentName) {
        try {
            const key = contributorsKey(documentName)
            const members = await redis.smembers(key)
            if (members && members.length > 0) await redis.del(key)
            return Array.isArray(members) ? members.filter((m) => m && m !== 'unknown') : []
        } catch (e) {
            log.error(`[Collab] Failed to read contributors for ${documentName}:`, e.message)
            return []
        }
    }

    let server = null
    // Docs that were opened with nothing in them (new docs): two people typing
    // the first lines of one at once each send a first block with nothing
    // beside it, which is not a second copy of anything (lineage.js).
    const startedEmpty = new Set()

    // tellGone stops the tabs of a doc that is gone (deleted, or archived by
    // the retention run, while it was open) from writing, and tells them to
    // open it again: the web app keeps their unsent edits aside, and opening
    // it again is refused as unavailable ("Couldn't open this doc").
    function tellGone(name) {
        const document = server?.hocuspocus?.documents?.get(name)
        if (!document) return
        for (const connection of document.getConnections()) {
            connection.readOnly = true
            try {
                connection.sendStateless(EPOCH_CHANGED_MESSAGE)
            } catch (e) {
                log.error(`[Collab] Could not tell a tab ${name} is gone:`, e?.message || e)
            }
        }
    }

    const failures = createSaveFailures({
        buffer,
        log,
        timers,
        ...(retryDelays ? { delays: retryDelays } : {}),
        ...(poisonAfter ? { poisonAfter } : {}),
        // A server-side connection: loads the doc if it is not in memory, and
        // saves it when it disconnects. Its user is "unknown", so it is never
        // counted as an editor.
        open: (name) => server.hocuspocus.openDirectConnection(name, { user: { id: 'unknown', name: 'Save retry' } }),
        onGone: tellGone,
    })

    async function loadBoard(name, id) {
        let response
        try {
            response = await api.get(`${backendUrl}/boardColab/getBoard/${id}`, internalHeaders())
        } catch (e) {
            throw refusal(`board ${id}`, e)
        }
        const board = response?.data?.data
        if (!board) throw refusal(`board ${id}`, Object.assign(new Error('the API returned no board'), { deterministic: true }))

        let ydoc = await buffer.unsaved(name)
        if (ydoc) {
            log.log(`[Collab] Opening board ${id} from the state its failed save kept`)
        } else {
            ydoc = new Y.Doc()
            const state = board.board_state
            if (typeof state === 'string' && state.length > 0) {
                try {
                    Y.applyUpdate(ydoc, Buffer.from(state, 'base64'))
                } catch (e) {
                    throw refusal(`board ${id}`, Object.assign(new Error(`its stored state does not decode: ${e.message}`), { deterministic: true }))
                }
                log.log(`[Collab] Loading existing board state for ${id} (${state.length} b64 chars)`)
            } else {
                log.log(`[Collab] No existing board state for ${id}, starting empty board`)
            }
        }
        await buffer.replay(name, ydoc)
        return ydoc
    }

    async function loadDoc(name, id) {
        let response
        try {
            response = await api.get(`${backendUrl}/docColab/getDoc/${id}`, internalHeaders())
        } catch (e) {
            throw refusal(`doc ${id}`, e)
        }
        const doc = response?.data?.data
        if (!doc) throw refusal(`doc ${id}`, Object.assign(new Error('the API returned no doc'), { deterministic: true }))
        const body = typeof doc.doc_body === 'string' ? doc.doc_body : ''

        // A save that failed before a restart kept the whole doc: open that, the
        // copy people were editing (its edits are not in the stored body yet).
        let ydoc = await buffer.unsaved(name)
        let empty = false
        if (ydoc) {
            log.log(`[Collab] Opening ${id} from the state its failed save kept`)
        } else if (body.trim().length > 0) {
            // The state it was last saved with, while the stored body is still
            // the one saved with it, so a browser holding that state syncs to the
            // same document instead of doubling it (docState.js); else the body.
            ydoc = savedStateFor(body, doc.doc_yjs_state, doc.doc_yjs_body_hash)
            if (ydoc) {
                log.log(`[Collab] Loading saved state for ${id}`)
            } else {
                // Rebuilt from the body: the same bytes for the same body every
                // time (lineage.js), and saved at once, so the next open uses it.
                let epoch
                try {
                    ;({ ydoc, epoch } = rebuildFromBody(id, body, extensions))
                } catch (e) {
                    throw refusal(`doc ${id}`, Object.assign(new Error(`its body could not be read: ${e.message}`), { deterministic: true }))
                }
                log.log(`[Collab] Rebuilt ${id} from its body (${body.length} chars, epoch ${epoch})`)
                saveRebuilt(id, ydoc, body)
            }
        } else {
            // Nothing stored yet (a new doc): empty is what it is.
            log.log(`[Collab] No existing content found for ${id}, creating empty doc`)
            ydoc = new Y.Doc()
            empty = true
        }
        // A buffered update that would add a second copy of the doc (a tab's
        // merge of an earlier lineage) is quarantined, not replayed. A doc
        // opened with nothing in it has no copy to double: everyone's first
        // lines in it are kept.
        if (empty) startedEmpty.add(name)
        else startedEmpty.delete(name)
        await buffer.replay(name, ydoc, { refuse: empty ? undefined : bringsSecondCopy })
        return ydoc
    }

    // saveRebuilt stores a just-rebuilt doc's state beside its body, only while
    // the body is still the one it was built from (expectBodyHash). Not waited
    // for: the rebuild is the same next time anyway.
    function saveRebuilt(id, ydoc, body) {
        let yjsState
        try {
            yjsState = encodeState(ydoc)
        } catch (e) {
            log.error(`[Collab] Could not encode the rebuilt state of ${id}:`, e?.message || e)
            return
        }
        // htmlContent is the same body: an API from before keepBody (mid-upgrade)
        // ignores the flag and writes back what it is given (storeDoc.js).
        api.post(`${backendUrl}/docColab/updateDoc`, { documentId: id, keepBody: true, yjsState, expectBodyHash: bodyHash(body), htmlContent: body }, internalHeaders())
            .then(() => log.log(`[Collab] Saved the rebuilt state of ${id}`))
            .catch((e) => log.error(`[Collab] Saving the rebuilt state of ${id} failed (the next open rebuilds the same copy):`, e?.message || e))
    }

    const post = (path) => async (payload) => {
        const response = await api.post(`${backendUrl}${path}`, payload, internalHeaders())
        log.log(`[Collab] Saved ${payload.documentId}${payload.keepBody ? ' (state only, body kept)' : ''}: ${response?.status}`)
    }

    server = new Server({
        ...serverOptions,
        // index.js owns SIGTERM, so pinned docs are let go before the server
        // waits for every doc to unload.
        stopOnSignals: false,
        // Periodic saves while people edit (5 s after the last change, at least
        // every 30 s); without them a doc saved only when its last editor left.
        debounce,
        maxDebounce,
        extensions: serverExtensions,

        // Server-side appends from the Go backend (agents writing into a doc),
        // POST /internal/docs/<id>/append (appendToDoc.js), and a saved state
        // written as HTML for the repair command, POST /internal/docs/<id>/render
        // (renderDoc.js). Each answers only its own path.
        onRequest: (() => {
            const append = createAppendHandler({
                getSecret,
                extensions,
                recordContributor: async (documentName, userId) => {
                    const key = contributorsKey(documentName)
                    await redis.sadd(key, userId)
                    await redis.expire(key, CONTRIBUTORS_TTL_SECONDS)
                },
            })
            const render = createRenderHandler({ getSecret, extensions })
            return async (data) => {
                await render(data)
                await append(data)
            }
        })(),

        async onAuthenticate(data) {
            const { token } = data
            if (!token) {
                throw new Error('Authentication token required')
            }
            const { kind, id } = parseDocumentName(data.documentName)

            // A read-only connection's edits are dropped by Hocuspocus. In
            // Hocuspocus 3 the flag lives on `connectionConfig`; this set
            // `data.connection.readOnly`, which no longer exists, so guests
            // and board viewers could write (a guest's editor, which lacks some
            // block types, deletes them from its copy and sends that).
            const readOnly = () => {
                if (data.connectionConfig) data.connectionConfig.readOnly = true
                if (data.connection) data.connection.readOnly = true
            }

            // A tab built against an older editor schema deletes the blocks it
            // does not know: it may read, not write, until it reloads
            // (schemaGate.js; told so once connected).
            const verdict = schemaVerdict(data.requestParameters?.get?.('schema'), schema)
            const outdated = kind === 'doc' && verdict === 'outdated'
            if (outdated) readOnly()

            try {
                const decoded = decodeToken(token)

                // Guest: a scoped, READ-ONLY external grant. A guest token
                // carries a `guest` marker; the guest authorize endpoint verifies
                // it and re-validates the grant (so revocation and expiry take
                // effect on every reconnect). Guests always join read-only.
                if (decoded && decoded.guest === true) {
                    const guestAuthUrl = kind === 'board'
                        ? `${backendUrl}/boardColab/guestAuthorize`
                        : `${backendUrl}/docColab/guestAuthorize`
                    const guestAuthBody = kind === 'board' ? { board_uuid: id } : { doc_uuid: id }
                    const gResp = await api.post(guestAuthUrl, guestAuthBody, { headers: { Authorization: `Bearer ${token}` } })
                    if (gResp.status !== 200) {
                        throw new Error('Unauthorized')
                    }
                    readOnly()
                    log.log(`[Collab] Guest joined ${kind} ${id} read-only`)
                    return { user: { id: decoded.sub || 'guest', name: decoded.name || 'Guest' }, schemaOutdated: outdated }
                }

                log.log(`[Collab] Authorizing user ${decoded?.sub} for ${kind} ${id}...`)
                const authUrl = kind === 'board' ? `${backendUrl}/boardColab/authorize` : `${backendUrl}/docColab/authorize`
                const authBody = kind === 'board' ? { board_uuid: id } : { doc_uuid: id }
                const response = await api.post(authUrl, authBody, { headers: { Authorization: `Bearer ${token}` } })
                log.log(`[Collab] Auth response for ${kind} ${id}: ${response.status}`)
                if (response.status !== 200) {
                    throw new Error('Unauthorized')
                }
                // Server-enforced read-only: for boards the authorize endpoint
                // returns canEdit. Viewers join the live session but Hocuspocus
                // rejects their writes. Docs use edit-access auth upstream.
                if (kind === 'board' && response.data && response.data.canEdit === false) {
                    readOnly()
                    log.log(`[Collab] Board ${id} joined read-only for user ${decoded?.sub}`)
                }
                if (outdated) log.log(`[Collab] ${id}: a tab with an older editor schema joined read-only (user ${decoded?.sub})`)
                return { user: { id: decoded?.sub || 'unknown', name: decoded?.name || 'Anonymous' }, schemaOutdated: outdated }
            } catch (error) {
                log.error('[Collab] Auth failed:', error.message)
                // A doc or board that is gone (deleted, archived, or never
                // there: the API answers 410 or 404) is refused as unavailable,
                // as a load that cannot read it is, so the web app says it
                // couldn't open it, with Retry, instead of reconnecting for
                // ever. Both answers give the same reason: it says no more than
                // the API's own 404 does.
                const status = error?.response?.status
                if (status === 404 || status === 410) {
                    const unavailable = new Error('Authentication failed')
                    unavailable.reason = UNAVAILABLE
                    throw unavailable
                }
                throw new Error('Authentication failed')
            }
        },

        // Throws rather than open what could not be read (see the top).
        async onLoadDocument(data) {
            const { kind, id } = parseDocumentName(data.documentName)
            try {
                const ydoc = kind === 'board' ? await loadBoard(data.documentName, id) : await loadDoc(data.documentName, id)
                // Opened anew (a doc let go as gone was restored): it saves again.
                failures.opened(data.documentName)
                return ydoc
            } catch (e) {
                log.error(`[Collab] REFUSED to open ${kind} ${id}: ${e.message}`)
                // Hocuspocus drops a document whose load failed without
                // destroying it, so its awareness timer would run for ever (one
                // per refused attempt while the API is down).
                try {
                    data.document?.destroy?.()
                } catch (destroyError) {
                    // nothing to keep
                }
                throw e.reason ? e : refusal(`${kind} ${id}`, e)
            }
        },

        // Never throws (see the top).
        async onStoreDocument(data) {
            const name = data.documentName
            const { kind, id } = parseDocumentName(name)
            // A gone doc is not saved at all, and one whose edits were just
            // quarantined not until someone edits it: letting go of its pin
            // saves it, and that save would be refused and start over
            // (saveFailures.js).
            if (failures.skipSave(name)) {
                log.log(`[Collab] Not saving ${name}: ${failures.isGone(name) ? 'it is gone (deleted or archived while open)' : 'its unsaved edits were quarantined and nothing changed since'}`)
                return
            }
            // The buffered updates this save will include, trimmed once it is
            // durable (counted before anything below changes the doc).
            const persisted = await buffer.count(name)
            try {
                const contributors = () => readAndClearContributors(name)
                const outcome = kind === 'board'
                    ? await persistBoard({ id, document: data.document, contributors, log, post: post('/boardColab/updateBoard') })
                    : await persistDoc({
                        id,
                        document: data.document,
                        extensions,
                        contributors,
                        log,
                        post: post('/docColab/updateDoc'),
                        storedBody: async () => (await api.get(`${backendUrl}/docColab/getDoc/${id}`, internalHeaders()))?.data?.data?.doc_body ?? null,
                    })
                if (outcome !== 'skipped') await buffer.trim(name, persisted)
                await failures.succeeded(name)
            } catch (err) {
                await failures.failed(name, err, data.document)
            }
        },

        async onChange(data) {
            // Nothing that reaches a gone doc can be saved (its tabs were made
            // read-only and told; this is what was on its way).
            if (failures.isGone(data.documentName)) return
            failures.edited(data.documentName)
            // Every update goes to the Redis buffer, so edits made between saves
            // survive a crash or a failed save.
            await buffer.push(data.documentName, data.update)
            // Who edited, for the next version's attribution.
            try {
                const userId = data?.context?.user?.id
                if (!userId || userId === 'unknown') return
                const key = contributorsKey(data.documentName)
                await redis.sadd(key, userId)
                await redis.expire(key, CONTRIBUTORS_TTL_SECONDS)
            } catch (e) {
                // attribution is best-effort
            }
        },

        // Tell a tab with an older editor schema to reload (it joined read-only).
        async connected(data) {
            if (data.context?.schemaOutdated) {
                try {
                    data.connection.sendStateless(SCHEMA_OUTDATED_MESSAGE)
                } catch (e) {
                    log.error('[Collab] Could not tell a tab to reload:', e?.message || e)
                }
            }
        },

        // A client sending a whole second copy of the doc (it still holds an
        // earlier lineage: lineage.js) may not write: its edits are dropped
        // from then on and it is told to drop its copy and open the doc again.
        async beforeHandleMessage(data) {
            if (parseDocumentName(data.documentName).kind !== 'doc') return
            // Opened with nothing in it: no earlier copy to double (see
            // startedEmpty above).
            if (startedEmpty.has(data.documentName)) return
            const connection = data.connection
            if (!connection || connection.readOnly) return
            const update = syncUpdateOf(data.update)
            if (!update || !bringsSecondCopy(update, data.document)) return
            connection.readOnly = true
            log.error(`[Collab] ${data.documentName}: a client sent a second copy of the doc; its edits are dropped and it is told to reload (user ${data.context?.user?.id})`)
            try {
                connection.sendStateless(EPOCH_CHANGED_MESSAGE)
            } catch (e) {
                log.error('[Collab] Could not tell a client to reload:', e?.message || e)
            }
        },

        // onConnect runs before the connection is counted (its payload has no
        // clientsCount), so it says only who is joining; onDisconnect counts.
        async onConnect(data) {
            log.log(`[Collab] Client connecting to ${data.documentName}`)
        },
        async onDisconnect(data) {
            log.log(`[Collab] Client disconnected from ${data.documentName} (remaining: ${data.clientsCount})`)
        },
        // afterUnloadDocument is the per-document hook ({ instance, documentName });
        // onDestroy is the server's and has no document.
        async afterUnloadDocument(data) {
            startedEmpty.delete(data.documentName)
            log.log(`[Collab] Document ${data.documentName} unloaded from memory`)
        },
        async onDestroy() {
            log.log('[Collab] Server destroyed — no longer accepting connections')
        },
    })

    return {
        server,
        buffer,
        failures,
        // Retry the docs whose saves were failing when the service stopped.
        async recoverUnsaved() {
            await failures.recover(await buffer.unsavedNames())
        },
        // Stop: no more retries, pinned docs let go, every doc saved and
        // unloaded (Hocuspocus's destroy saves what is pending).
        async stop() {
            await failures.shutdown()
            await server.destroy()
        },
    }
}
