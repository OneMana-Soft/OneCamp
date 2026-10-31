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
import { createUpdateBuffer } from './buffer.js'
import { createContributors, splitContributors } from './contributors.js'
import { bodyHash, encodeState, savedStateFor } from './docState.js'
import { COLLAB_EXTENSIONS } from './extensions.js'
import { createPersistence, persistedMessage } from './persisted.js'
import { createSaveFailures } from './saveFailures.js'
import { EPOCH_CHANGED_MESSAGE, bringsSecondCopy, epochIn, rebuildFromBody, syncUpdateOf } from './lineage.js'
import { SCHEMA_HASH, SCHEMA_OUTDATED_MESSAGE, schemaVerdict } from './schemaGate.js'
import { authorizeDoc, sessionMessage } from './session.js'
import { persistBoard } from './storeBoard.js'
import { persistDoc } from './storeDoc.js'
import { guardIncoming } from './presenceGuard.js'
import { createBlockIdsTransform } from './transforms/blockIds.js'
import { createLegacyContentTransform } from './transforms/legacyContent.js' // B2
import { closeWhenLinkEnds } from './live/ops/reauth.js'
import { trackGuestStrokes } from './live/ops/guestStrokes.js'

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
// "edited by" is a SET of people (contributors.js).
export { CONTRIBUTORS_TTL_SECONDS, contributorsKey } from './contributors.js'

// storedAtOf is when a doc as the API sends it was last stored (its body's
// doc_updated_at), for the "persisted" message a tab hears as it connects;
// the time it is read when the API does not say (the zero time is no time).
export function storedAtOf(doc) {
    const t = Date.parse(doc?.doc_updated_at ?? '')
    return Number.isFinite(t) && t > 0 ? t : Date.now()
}

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
    const contributors = createContributors(redis, { log })
    // The last state the API stored of each open doc (persisted.js).
    const persistence = createPersistence()
    const internalHeaders = () => ({ headers: { 'X-Internal-Secret': getSecret() } })

    // A QUIET WRITE'S OWN SAVE (live/ops/write.js). A quiet write (a copy's
    // body, an import's) is stored at once, but a save asks Redis before it
    // reads the doc (the Redis extension's lock, then the buffer's count), and
    // whatever reached the doc meanwhile went into the quiet save with it: an
    // @mention someone typed then told nobody (21 Oct 2026). So the op hands
    // over the doc as it left it, taken in the same step as its write, and the
    // doc's next save stores that first, quiet and credited to the quiet write
    // alone. Then it stores the doc as it is now, as any save does, if that
    // holds more: loudly, credited to whoever else edited, and checked against
    // the quiet save (what that save stored is its last). Nothing joined, one
    // save. A snapshot is dropped as soon as its save is in, and one whose save
    // failed is kept for the retry. Kept in memory, so it is gone once the doc
    // is let go unsaved (a restart, or its edits quarantined): the quiet mark
    // in the contributors set is all that is left then (contributors.js).
    const quietWrites = new Map() // name -> { doc: a copy of the doc, members: Set of quiet members }
    function holdQuietWrite(name, doc, member) {
        if (!member) return
        const snapshot = new Y.Doc()
        Y.applyUpdate(snapshot, Y.encodeStateAsUpdate(doc))
        // A second quiet write before the first is stored: the later copy holds
        // both, and its save is credited to both.
        const members = new Set(quietWrites.get(name)?.members)
        members.add(member)
        quietWrites.set(name, { doc: snapshot, members })
    }

    // noteStored records a doc save the API confirmed, and tells the doc's tabs.
    function noteStored(name, document, saved) {
        if (saved.outcome !== 'saved' && saved.outcome !== 'state-only' && saved.outcome !== 'kept-body') return
        const at = Date.now()
        persistence.stored(name, { at, stateVector: saved.stateVector, hash: saved.hash, lineage: saved.lineage })
        // Saved means stored (I8): the tabs' save dots follow this.
        try {
            document.broadcastStateless(persistedMessage({ at, stateVector: saved.stateVector, lineage: saved.lineage }))
        } catch (e) {
            log.error(`[Collab] Could not tell ${name}'s tabs it is stored:`, e?.message || e)
        }
    }

    // A board's save takes everyone who edited it so far, as before.
    async function boardContributors(documentName) {
        const members = await contributors.peek(documentName)
        await contributors.ack(documentName, members)
        return splitContributors(members).users
    }

    let server = null
    // Docs that were opened with nothing in them (new docs): two people typing
    // the first lines of one at once each send a first block with nothing
    // beside it, which is not a second copy of anything (lineage.js).
    const startedEmpty = new Set()
    // The connections whose leaving was said (onDisconnect).
    const reported = new WeakSet()
    // The connections whose first sync was said (beforeSync).
    const firstSynced = new WeakSet()

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

    // Block ids for a doc written before blocks had them, given once as it
    // loads (transforms/blockIds.js), as an edit whose user is "unknown", so
    // it is never counted as an editor.
    const blockIds = createBlockIdsTransform({ extensions, log })
    // Old toggles and colours moved over as a doc loads, before its blocks
    // get ids (transforms/legacyContent.js, B2).
    const legacyContent = createLegacyContentTransform({ log })
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
                // What is stored already: a save with nothing new sends nothing,
                // and a tab opening it hears when it was stored.
                persistence.stored(name, { at: storedAtOf(doc), stateVector: Y.encodeStateVector(ydoc), hash: doc.doc_yjs_body_hash, lineage: epochIn(ydoc) || '' })
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
                saveRebuilt(name, id, ydoc, body)
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
    function saveRebuilt(name, id, ydoc, body) {
        let yjsState
        try {
            yjsState = encodeState(ydoc)
        } catch (e) {
            log.error(`[Collab] Could not encode the rebuilt state of ${id}:`, e?.message || e)
            return
        }
        const stateVector = Y.encodeStateVector(ydoc)
        const lineage = epochIn(ydoc) || ''
        // htmlContent is the same body: an API from before keepBody (mid-upgrade)
        // ignores the flag and writes back what it is given (storeDoc.js).
        api.post(`${backendUrl}/docColab/updateDoc`, { documentId: id, keepBody: true, yjsState, expectBodyHash: bodyHash(body), htmlContent: body, lineage }, internalHeaders())
            .then((response) => {
                if (response?.data?.data?.stored === false) return
                if (!persistence.get(name)) persistence.stored(name, { stateVector, hash: bodyHash(body), lineage })
                log.log(`[Collab] Saved the rebuilt state of ${id}`)
            })
            .catch((e) => log.error(`[Collab] Saving the rebuilt state of ${id} failed (the next open rebuilds the same copy):`, e?.message || e))
    }

    const post = (path) => async (payload) => {
        const response = await api.post(`${backendUrl}${path}`, payload, internalHeaders())
        log.log(`[Collab] Saved ${payload.documentId}${payload.keepBody ? ' (state only, body kept)' : ''}: ${response?.status}`)
        return response?.data?.data ?? null
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

        // THE PUBLIC PORT TAKES NO SERVER-SIDE WRITES. Appends and renders
        // were answered here, behind the internal secret alone, on the port
        // Traefik publishes. They are Live Doc Writer ops now, on the internal
        // port (live/server.js); a caller still pointed here is told so instead
        // of getting Hocuspocus's welcome page with a 200.
        async onRequest({ request, response }) {
            const path = (request.url || '').split('?')[0]
            if (!path.startsWith('/internal/') && !path.startsWith('/live/')) return
            response.writeHead(404, { 'Content-Type': 'application/json' })
            response.end(JSON.stringify({ msg: 'server-side writes are on the internal port (LIVE_PORT)' }))
            // Answered; stop Hocuspocus's default reply.
            throw null
        },

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

                // Guest: a scoped external grant. A guest token carries a
                // `guest` marker; the guest authorize endpoint verifies it and
                // re-validates the grant (so revocation and expiry take effect
                // on every reconnect). A share link's guest reads only; a
                // call's guest draws on the board on its stage when the call
                // lets them, as the endpoint answers (canEdit).
                if (decoded && decoded.guest === true) {
                    const guestAuthUrl = kind === 'board'
                        ? `${backendUrl}/boardColab/guestAuthorize`
                        : `${backendUrl}/docColab/guestAuthorize`
                    const guestAuthBody = kind === 'board' ? { board_uuid: id } : { doc_uuid: id }
                    const gResp = await api.post(guestAuthUrl, guestAuthBody, { headers: { Authorization: `Bearer ${token}` } })
                    if (gResp.status !== 200) {
                        throw new Error('Unauthorized')
                    }
                    const canEdit = kind === 'board' && gResp.data?.canEdit === true
                    if (!canEdit) readOnly()
                    log.log(`[Collab] Guest joined ${kind} ${id}${canEdit ? '' : ' read-only'}`)
                    // When the link stops working, if it ever does: the
                    // socket closes then (connected, below).
                    const linkEndsAt = Date.parse(gResp.data?.until)
                    return {
                        user: { id: decoded.sub || 'guest', name: decoded.name || 'Guest' },
                        guest: true,
                        schemaOutdated: outdated,
                        access: { level: canEdit ? 'edit' : 'view', canEdit, locked: false },
                        ...(Number.isFinite(linkEndsAt) ? { linkEndsAt } : {}),
                    }
                }

                // A member on a doc: everyone who may read joins live (the Doc
                // Session, session.js); a reader's connection is read-only.
                if (kind === 'doc') {
                    const access = await authorizeDoc({ api, backendUrl, token, id, secret: getSecret() })
                    if (!access.canEdit) readOnly()
                    log.log(`[Collab] ${id}: user ${decoded?.sub} joined as ${access.level}${access.canEdit && !outdated ? '' : ' (read-only)'}`)
                    // Named as the API answers (a session token carries no name): presence wears it (presenceGuard.js).
                    return { user: { id: decoded?.sub || 'unknown', name: access.name || decoded?.name || 'Anonymous' }, schemaOutdated: outdated, access }
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
                const named = typeof response.data?.name === 'string' && response.data.name ? response.data.name : null
                return { user: { id: decoded?.sub || 'unknown', name: named || decoded?.name || 'Anonymous' }, schemaOutdated: outdated }
            } catch (error) {
                log.error('[Collab] Auth failed:', error.message)
                // The tab is told why (session.js REFUSED), so it can fetch a
                // fresh token, say the doc is gone, or stay on the saved copy.
                // A member's doc that is gone or never there is refused here
                // as 'gone' or 'not-found', which the web app takes as final.
                if (error?.reason) throw Object.assign(new Error('Authentication failed'), { reason: error.reason })
                // Anything else that is gone (a board, a guest's doc: deleted,
                // archived, or never there, so the API answers 410 or 404) is
                // refused as unavailable, as a load that cannot read it is, so
                // the web app says it couldn't open it, with Retry, instead of
                // reconnecting for ever. Both answers give the same reason: it
                // says no more than the API's own 404 does.
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

        // A board starts keeping who of a call's guests drew what. A doc whose
        // blocks have no ids gets them, as one edit, before anyone syncs
        // (transforms/blockIds.js). Never throws: a doc without ids opens as
        // it always did.
        async afterLoadDocument(data) {
            if (parseDocumentName(data.documentName).kind === 'board') {
                // Who of a call's guests drew what (live/ops/guestStrokes.js).
                trackGuestStrokes(data.document)
                return
            }
            if (parseDocumentName(data.documentName).kind !== 'doc') return
            try {
                legacyContent.give(data.documentName, data.document)
            } catch (e) {
                log.error(`[Collab] ${data.documentName}: moving its old toggles and colours over failed:`, e?.message || e)
            }
            try {
                blockIds.give(data.documentName, data.document)
            } catch (e) {
                log.error(`[Collab] ${data.documentName}: giving its blocks ids failed:`, e?.message || e)
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
                if (kind === 'board') {
                    const outcome = await persistBoard({ id, document: data.document, contributors: () => boardContributors(name), log, post: post('/boardColab/updateBoard') })
                    if (outcome !== 'skipped') await buffer.trim(name, persisted)
                } else {
                    // One save of `document`, checked against the last one stored.
                    const save = (document, who) => persistDoc({
                        id,
                        document,
                        extensions,
                        contributors: who,
                        last: persistence.get(name),
                        log,
                        post: post('/docColab/updateDoc'),
                        storedBody: async () => (await api.get(`${backendUrl}/docColab/getDoc/${id}`, internalHeaders()))?.data?.data?.doc_body ?? null,
                    })
                    // A quiet write's own save first, and the next one's if
                    // another came while it was stored (see quietWrites).
                    const taken = new Set()
                    for (let held = quietWrites.get(name); held; held = quietWrites.get(name)) {
                        const members = [...held.members]
                        const quiet = await save(held.doc, { peek: async () => members, ack: (sent) => contributors.ack(name, sent) })
                        if (quietWrites.get(name) === held) quietWrites.delete(name)
                        // A save that sent nothing took no mark: clear it here.
                        if (quiet.outcome === 'unchanged' || quiet.outcome === 'skipped') await contributors.ack(name, members)
                        for (const m of members) taken.add(m)
                        noteStored(name, data.document, quiet)
                    }
                    // The doc as it is now. A quiet mark is never this save's
                    // when a snapshot accounts for it, even if clearing it
                    // failed: it is cleared again with whom this save credits.
                    const saved = await save(data.document, {
                        peek: async () => (await contributors.peek(name)).filter((m) => !taken.has(m) && !quietWrites.get(name)?.members.has(m)),
                        ack: (sent) => contributors.ack(name, [...sent, ...taken]),
                    })
                    if (saved.outcome !== 'skipped') await buffer.trim(name, persisted)
                    noteStored(name, data.document, saved)
                }
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
            // Who edited, for the next version's attribution. A Live Doc
            // Writer op records its own (live/ops/write.js), with the agent and
            // the asker; its direct connection is skipped here.
            const userId = data?.context?.user?.id
            if (!userId || userId === 'unknown' || data?.context?.liveDocWriter) return
            await contributors.record(data.documentName, { user: userId })
        },

        // Tell a doc's tab what the server decided (the Doc Session: its level,
        // whether it may write, the doc's lineage), a tab with an older editor
        // schema to reload (it joined read-only), and every doc tab what is
        // stored already, so its save dot knows this server says when edits are
        // stored (persisted.js).
        async connected(data) {
            // A guest's link that runs out while it is open closes the socket
            // when it does; the reconnect is refused (the API checks a link's
            // end on every authorisation).
            if (data.context?.linkEndsAt && data.connection) closeWhenLinkEnds(data.connection, data.context.linkEndsAt)
            if (data.context?.access && parseDocumentName(data.documentName).kind === 'doc') {
                try {
                    const document = data.connection?.document
                    data.connection.sendStateless(sessionMessage({
                        access: data.context.access,
                        lineage: document ? epochIn(document) : null,
                        schemaVersion: SCHEMA_HASH,
                        readOnly: !!data.connection?.readOnly || !!data.context.schemaOutdated,
                    }))
                } catch (e) {
                    log.error('[Collab] Could not tell a tab its session:', e?.message || e)
                }
            }
            if (data.context?.schemaOutdated) {
                try {
                    data.connection.sendStateless(SCHEMA_OUTDATED_MESSAGE)
                } catch (e) {
                    log.error('[Collab] Could not tell a tab to reload:', e?.message || e)
                }
            }
            const stored = persistence.get(data.documentName)
            if (stored && parseDocumentName(data.documentName).kind === 'doc') {
                try {
                    data.connection.sendStateless(persistedMessage(stored))
                } catch (e) {
                    log.error('[Collab] Could not tell a tab what is stored:', e?.message || e)
                }
            }
        },

        // A client sending a whole second copy of the doc (it still holds an
        // earlier lineage: lineage.js) may not write: its edits are dropped
        // from then on and it is told to drop its copy and open the doc again.
        async beforeHandleMessage(data) {
            // Presence and broadcasts first, on every doc and board: who a tab
            // is comes from its sign-in, a guest who may only look publishes
            // nothing, and a client's broadcast reaches no tab (presenceGuard.js).
            guardIncoming(data)
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
        // Its time joins the connection's context, for its first sync's line.
        async onConnect(data) {
            log.log(`[Collab] Client connecting to ${data.documentName} (connection ${data.socketId})`)
            return { connectedAt: Date.now() }
        },
        // A connection's first sync, said once it is answered: a tab that
        // signs in and never gets its doc then shows which way the round trip
        // failed (23 Oct 2026: on the demo the server joined and loaded a doc
        // in 13 ms, and nothing said whether the tab's sync step 1 came). The
        // answer is sent before this runs: Hocuspocus 3.4.3 calls the hook
        // without waiting for it, and answers step 1 in the same step.
        async beforeSync(data) {
            // 0: sync step 1, the state vector a tab opens its sync with.
            if (data.type !== 0 || !data.connection || firstSynced.has(data.connection)) return
            firstSynced.add(data.connection)
            log.log(`[Collab] ${data.documentName}: answered the first sync of connection ${data.connection.socketId}, ${Date.now() - data.context.connectedAt} ms after it connected`)
        },
        // Once per connection. Hocuspocus 3.4.3 closes a connection twice when
        // its socket goes while the tab's presence is set:
        // Document.removeConnection removes the presence before it deletes the
        // connection, the removal is sent to that connection too, and the
        // failed send closes it again. Each connection has its own context
        // (one per socket and doc; a direct connection's is the one it was
        // opened with, if any: it is closed once either way).
        async onDisconnect(data) {
            if (data.context) {
                if (reported.has(data.context)) return
                reported.add(data.context)
            }
            log.log(`[Collab] Client disconnected from ${data.documentName} (remaining: ${data.clientsCount})`)
        },
        // afterUnloadDocument is the per-document hook ({ instance, documentName });
        // onDestroy is the server's and has no document.
        async afterUnloadDocument(data) {
            startedEmpty.delete(data.documentName)
            persistence.forget(data.documentName)
            quietWrites.delete(data.documentName)
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
        persistence,
        // What the Live Doc Writer's ops share (live/server.js, live/routes.js).
        live: { extensions, contributors, persistence, log, isGone: (name) => failures.isGone(name), quiet: { hold: holdQuietWrite } },
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
