// Saving a doc: its HTML body, the text it holds and its Yjs state, in one
// request the backend stores in one mutation (business/Doc
// UpdateDocFromCollab).
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

import { TiptapTransformer } from '@hocuspocus/transformer'
import { encodeState } from './docState.js'
import { RenderError, renderDocHTML } from './preserved.js'

// textOf is a doc's plain text, for search.
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

// persistDoc saves one doc. `post(payload)` sends it to the backend and throws
// on failure; `contributors()` is who edited it since the last save. Returns
// what happened: 'saved', 'kept-body', or 'skipped' (empty or unreadable).
//
// `storedBody()` is the body the backend holds now. A keep-body save sends it
// as the HTML too: a backend from before keepBody (during an upgrade, the API
// can still be the old one) ignores the flag and saves the HTML it is given,
// and with no HTML it saved an empty body. Given the stored one, it writes the
// same body back. Without it, nothing is sent and the save is retried.
export async function persistDoc({ id, document, extensions, post, contributors = async () => [], log = console, render, storedBody = async () => null }) {
    let json
    try {
        // The body only: the doc also holds a `meta` map (lineage.js).
        json = TiptapTransformer.fromYdoc(document, 'default')
    } catch (e) {
        // The same document fails the same way every time: not worth retrying
        // for ever (saveFailures.js).
        throw Object.assign(new Error(`doc ${id}: its Yjs document could not be read: ${e.message}`), { deterministic: true })
    }

    if (!json) {
        log.warn(`[Collab] Attempted to save empty/invalid document for ${id}`)
        return 'skipped'
    }
    // Defensive: an empty doc is never saved over a stored one.
    if (isEmptyDoc(json)) {
        log.log(`[Collab] Document ${id} is empty, skipping save to avoid overwriting content`)
        return 'skipped'
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

    let rendered
    try {
        rendered = renderDocHTML(json, extensions, render)
    } catch (e) {
        if (!(e instanceof RenderError)) throw e
        log.error(`[Collab] DOC ${id}: ${e.message}. The stored body is KEPT; ${yjsState ? 'saving the Yjs state only' : 'nothing saved, edits stay in memory and the Redis buffer'}.`)
        if (!yjsState) throw e
        const body = await storedBody().catch(() => null)
        if (typeof body !== 'string' || body.trim() === '') throw e
        await post({ documentId: id, keepBody: true, yjsState, htmlContent: body, contributors: await contributors() })
        return 'kept-body'
    }
    if (rendered.preserved.length || rendered.degraded) {
        log.error(`[Collab] DOC ${id}: this service's schema lacks ${rendered.preserved.join(', ') || 'nothing'}` +
            `${rendered.degraded ? `, and ${rendered.degraded} block(s) were saved as text only` : ''}; ` +
            'kept them in the body as they are. Update the collaboration service (extensions.js, schema-manifest.json).')
    }

    log.log(`[Collab] Saving document ${id} to backend...`)
    await post({
        documentId: id,
        content: json,
        textContent: textOf(json),
        htmlContent: rendered.html,
        yjsState,
        contributors: await contributors(),
    })
    return 'saved'
}
