// Saving a board: its whole Yjs state (Excalidraw elements), base64, with a
// short snippet. No HTML: a board is canvas data.

import * as Y from 'yjs'

// Compaction: deleted Excalidraw elements stay in the Yjs map as isDeleted
// tombstones. Without pruning, a heavily-edited board grows forever (slower
// loads, bigger payloads). Tombstones older than this window (well beyond the
// undo horizon) are dropped on the server during a save, so the saved + synced
// board stays bounded to live content.
export const TOMBSTONE_PRUNE_AGE_MS = 10 * 60 * 1000

// persistBoard saves one board. `post(payload)` sends it to the backend and
// throws on failure. Returns 'saved' or 'skipped' (a brand-new empty board);
// throws when it cannot be saved, with `deterministic` set when it never will.
export async function persistBoard({ id, document, post, contributors = async () => [], log = console, now = Date.now }) {
    try {
        const elementsMap = document.getMap('elements')
        const t = now()
        const stale = []
        elementsMap.forEach((val, key) => {
            if (val && val.isDeleted === true && typeof val.updated === 'number' && t - val.updated > TOMBSTONE_PRUNE_AGE_MS) {
                stale.push(key)
            }
        })
        if (stale.length > 0) {
            document.transact(() => {
                for (const k of stale) elementsMap.delete(k)
            }, 'prune')
            log.log(`[Collab] Pruned ${stale.length} deleted element(s) from board ${id}`)
        }
    } catch (e) {
        log.error(`[Collab] Tombstone prune failed for ${id}:`, e.message)
    }

    let stateB64 = ''
    try {
        stateB64 = Buffer.from(Y.encodeStateAsUpdate(document)).toString('base64')
    } catch (e) {
        throw Object.assign(new Error(`board ${id}: its state could not be encoded: ${e.message}`), { deterministic: true })
    }

    // Skip persisting a brand-new empty board (no elements yet) to avoid
    // clobbering content on transient connections. The canvas stores elements
    // in a Y.Map keyed by element id (see BoardCanvas).
    const elements = document.getMap('elements')
    const elementCount = elements ? elements.size : 0
    if (elementCount === 0 && stateB64.length < 40) {
        log.log(`[Collab] Board ${id} is empty, skipping save`)
        return 'skipped'
    }

    log.log(`[Collab] Saving board ${id} to backend...`)
    await post({ documentId: id, state: stateB64, snippet: `${elementCount} elements`, contributors: await contributors() })
    return 'saved'
}
