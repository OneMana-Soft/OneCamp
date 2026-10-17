// Saving a doc: its HTML body and its Yjs state, in one request the API stores
// in one mutation (business/Doc SaveFromCollab).
//
// THE RULE: a failure never replaces good content.
//   - The body is written by renderDocHTML (preserved.js), which keeps a block
//     the schema lacks instead of failing on it.
//   - If even that cannot write the body, the stored body is kept, the failure
//     is logged with the doc's id, and only the Yjs state is saved (keepBody),
//     so the next open still has every edit.
//   - If nothing could be saved, this throws, and the caller keeps the edits
//     and tries again (saveFailures.js); it never reaches Hocuspocus.
// Never a placeholder: until 9 Oct a failed render saved
// "<p>Error generating HTML</p>" over the whole doc.
//
// HASH FIRST (invariants I15 and I8). Every save used to post the body, its
// text, its JSON and its state, about six times the HTML, and the API fanned
// each one out at full cost (search, the AI index, permissions, comments) every
// 5 to 30 seconds while anyone typed. Now:
//   - the request carries only the body, the state, the lineage and who edited;
//   - a body that hashes the same as the last one stored is a state-only save
//     (keepBody, against that hash): the API stores the state without touching
//     anything else (the body rides along only for an API mid-upgrade, below);
//   - a doc whose state has not moved since the last save sends nothing.
//
// AN EMPTIED DOC IS SAVED. An empty doc used to be skipped outright, to keep a
// failed load (served empty) from being saved over the stored body. A failed
// load is refused now (collabServer.js), so the skip only lost real work: a doc
// someone emptied came back on its next open. It is saved when its body holds
// deleted content, which is what emptying it leaves; a doc that never had any
// is still not saved.

import * as Y from 'yjs'
import { TiptapTransformer } from '@hocuspocus/transformer'
import { bodyHash, encodeState } from './docState.js'
import { splitContributors } from './contributors.js'
import { epochIn } from './lineage.js'
import { sameState } from './persisted.js'
import { RenderError, renderDocHTML } from './preserved.js'

// textOf is a doc's plain text.
export const textOf = (node) => {
    let text = ''
    if (node.type === 'text') {
        text += node.text
    }
    if (node.content) {
        node.content.forEach(child => {
            text += textOf(child) + ' '
        })
    }
    return text.trim()
}

export function isEmptyDoc(json) {
    return !json.content ||
        json.content.length === 0 ||
        (json.content.length === 1 &&
            json.content[0].type === 'paragraph' &&
            (!json.content[0].content || json.content[0].content.length === 0))
}

// emptiedByEdit is whether a doc's body holds deleted content: it is empty
// because someone deleted what it held, not because nothing was ever written.
export function emptiedByEdit(ydoc) {
    const walk = (type) => {
        for (let item = type._start; item; item = item.right) {
            if (item.deleted) return true
            const inner = item.content?.type
            if (inner && inner._start !== undefined && walk(inner)) return true
        }
        return false
    }
    return walk(ydoc.getXmlFragment('default'))
}

// No contributors: what a caller that tracks none passes.
const NOBODY = { peek: async () => [], ack: async () => {} }

// persistDoc saves one doc.
//   post(payload)   sends it to the API and throws on failure; it answers the
//                   API's data ({stored} for a state-only save);
//   contributors    peek() is who edited since the last save (the set's
//                   members), ack(members) clears them once stored;
//   last            the last state the API stored ({stateVector, hash}), from
//                   persisted.js, or null;
//   storedBody()    the body the API holds now, for a save that keeps it.
// Returns { outcome, stateVector, hash, lineage }: outcome is 'saved',
// 'state-only', 'kept-body', 'unchanged' (nothing sent) or 'skipped' (an empty
// doc that never held anything); stateVector is the state the save holds.
//
// A SAVE THAT KEEPS THE BODY STILL CARRIES ONE. An API from before keepBody
// (during an upgrade, the API can still be the old one) ignores the flag and
// saves the HTML it is given, and with no HTML it saved an empty body. So a
// state-only save sends the body it hashes to (the stored one), and a kept-body
// save the body storedBody() reads, which such an API writes back unchanged;
// the current API ignores it. Without a stored body to send, nothing is sent
// and the save is retried.
export async function persistDoc({ id, document, extensions, post, contributors = NOBODY, last = null, log = console, render, storedBody = async () => null }) {
    let json
    try {
        // The body only: the doc also holds a `meta` map (lineage.js) and the
        // record of agents' edits (live/ops/write.js).
        json = TiptapTransformer.fromYdoc(document, 'default')
    } catch (e) {
        // The same document fails the same way every time: not worth retrying
        // for ever (saveFailures.js).
        throw Object.assign(new Error(`doc ${id}: its Yjs document could not be read: ${e.message}`), { deterministic: true })
    }

    if (!json) {
        log.warn(`[Collab] Attempted to save empty/invalid document for ${id}`)
        return { outcome: 'skipped', stateVector: null, hash: null, lineage: '' }
    }
    if (isEmptyDoc(json) && !emptiedByEdit(document)) {
        log.log(`[Collab] Document ${id} has never held anything; nothing to save`)
        return { outcome: 'skipped', stateVector: null, hash: null, lineage: '' }
    }

    // The state and its vector, taken together and before anything is awaited,
    // so the vector names exactly what the stored state holds.
    const stateVector = Y.encodeStateVector(document)
    const lineage = epochIn(document) || ''
    if (last && last.hash && sameState(last.stateVector, stateVector)) {
        return { outcome: 'unchanged', stateVector, hash: last.hash, lineage }
    }
    // Saved beside the body, so the next load opens the same document
    // (docState.js). A state that won't encode is left out, and the next load
    // rebuilds from the body as before.
    let yjsState = ''
    try {
        yjsState = encodeState(document)
    } catch (e) {
        log.error(`[Collab] Failed to encode the state of ${id}:`, e.message)
    }

    const members = await contributors.peek()
    const { users, agents } = splitContributors(members)
    const who = { contributors: users, agents }

    let rendered
    try {
        rendered = renderDocHTML(json, extensions, render)
    } catch (e) {
        if (!(e instanceof RenderError)) throw e
        log.error(`[Collab] DOC ${id}: ${e.message}. The stored body is KEPT; ${yjsState ? 'saving the Yjs state only' : 'nothing saved, edits stay in memory and the Redis buffer'}.`)
        if (!yjsState) throw e
        const body = await storedBody().catch(() => null)
        if (typeof body !== 'string' || body.trim() === '') throw e
        await post({ documentId: id, keepBody: true, yjsState, htmlContent: body, lineage, ...who })
        await contributors.ack(members)
        return { outcome: 'kept-body', stateVector, hash: null, lineage }
    }
    if (rendered.preserved.length || rendered.degraded) {
        log.error(`[Collab] DOC ${id}: this service's schema lacks ${rendered.preserved.join(', ') || 'nothing'}` +
            `${rendered.degraded ? `, and ${rendered.degraded} block(s) were saved as text only` : ''}; ` +
            'kept them in the body as they are. Update the collaboration service (extensions.js, schema-manifest.json).')
    }
    const hash = bodyHash(rendered.html)

    // The same body as the last save: only the state moved (an edit undone, a
    // record beside the body). The API stores the state against that body's
    // hash and nothing else; if the stored body is not that one any more, it
    // says so and the whole body goes after all. The HTML is that same body,
    // for an API from before keepBody (above).
    if (last && last.hash === hash && yjsState) {
        const answer = await post({ documentId: id, keepBody: true, yjsState, expectBodyHash: hash, htmlContent: rendered.html, lineage, ...who })
        if (answer?.stored !== false) {
            await contributors.ack(members)
            return { outcome: 'state-only', stateVector, hash, lineage }
        }
        log.log(`[Collab] ${id}: the stored body is not the one last saved; saving it whole`)
    }

    log.log(`[Collab] Saving document ${id} to backend...`)
    await post({ documentId: id, htmlContent: rendered.html, yjsState, lineage, ...who })
    await contributors.ack(members)
    return { outcome: 'saved', stateVector, hash, lineage }
}
