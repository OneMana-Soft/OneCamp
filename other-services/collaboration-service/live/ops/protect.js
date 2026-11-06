// Protected objects on a board (miro-44: Miro's Protected Lock, there on its
// Enterprise plan, here on every plan).
//
// WHAT IS PROTECTED. Elements are whole objects in one shared map, merged by
// their `version`, and Excalidraw's own `locked` flag is a tab-side nicety
// anyone with edit can clear. So this service keeps its own map beside them,
// "protected": an element's id -> { by, name, at }, who protected it and when.
// Only the API's protect route writes it (the protect op below, once the API
// has checked that the person may protect and written the audit row), and a
// shape's bound text is protected with it.
//
// THE GUARD. trackProtected puts back any socket's change to a protected
// element, its owner's included (protection is lifted first, then the object
// is edited, as in Miro): an edit, a deletion or an unlock returns as it was,
// as a newer version so every tab takes it, and text bound onto a protected
// shape is taken off: new text is deleted, and text already on the board goes
// back to where it was (V-528). A socket's write to the record itself is put
// back as it was. A change is repaired, never dropped: dropping a Yjs update would leave
// that tab's later updates waiting for ever.

import { openDoc } from './write.js'
import { putBackSocketWrites } from './mapGuard.js'

export const PROTECTED = 'protected'

// The origin of this service's own changes to protected elements and their
// record.
export const PROTECT_ORIGIN = Object.freeze({ protect: true })

// The most objects one protect op names (the API's route says the same).
export const MAX_PROTECT = 500

const ELEMENTS = 'elements'

const refuse = (status, msg) => Object.assign(new Error(msg), { status })

// newer is v as a version every tab takes over the ones given.
function newer(v, ...against) {
    const version = Math.max(v.version || 0, ...against.map((a) => a?.version || 0)) + 1
    return { ...v, version, versionNonce: Math.floor(Math.random() * 2 ** 31), updated: Date.now() }
}

// trackProtected keeps a board's protected elements, and the record of them,
// as they are against every socket. Changes with no socket behind them
// (loading, this service's ops, pruning) stand.
export function trackProtected(doc) {
    const elements = doc.getMap(ELEMENTS)
    const record = doc.getMap(PROTECTED)
    elements.observe((event) => {
        if (!event.transaction.origin?.context || record.size === 0) return
        const putBack = []
        event.changes.keys.forEach((change, id) => {
            const now = elements.get(id)
            if (record.has(id)) {
                if (change.oldValue) putBack.push([id, newer(change.oldValue, now)])
            } else if (now?.containerId && record.has(now.containerId) && now.isDeleted !== true) {
                // Bound onto a protected shape by this change: new text is taken off, and text that was already on
                // the board (bound elsewhere, free, or a deleted label brought back by an undo) is put back as it
                // was. Only text that was bound there AND alive before this change stands.
                const was = change.oldValue
                if (change.action === 'add') putBack.push([id, newer({ ...now, isDeleted: true })])
                else if (was && (was.containerId !== now.containerId || was.isDeleted === true)) putBack.push([id, newer(was, now)])
            }
        })
        if (putBack.length === 0) return
        doc.transact(() => {
            for (const [id, v] of putBack) elements.set(id, v)
        }, PROTECT_ORIGIN)
    })
    // The record is this service's alone: any socket's write to it is put back.
    putBackSocketWrites(doc, record, PROTECT_ORIGIN)
}

// isProtected is whether a board's live copy holds an element protected.
export function isProtected(doc, id) {
    return doc.getMap(PROTECTED).has(id)
}

// protect locks (body.protect true) or lifts (false) the elements named in
// body.element_ids, with the text bound to them, in one transaction, as
// body.by.user_uuid (body.name is how the board names them). An id the board
// doesn't have, or has as asked already, changes nothing. Answers how many of
// the named elements changed.
export async function protect({ docName, body, hocuspocus }) {
    const ids = body?.element_ids
    if (!Array.isArray(ids) || ids.length === 0) throw refuse(400, 'no objects named')
    if (ids.length > MAX_PROTECT) throw refuse(400, `at most ${MAX_PROTECT} objects at once`)
    if (!ids.every((id) => typeof id === 'string' && id !== '' && id.length <= 64)) throw refuse(400, 'an object id is not one')
    if (typeof body.protect !== 'boolean') throw refuse(400, 'protect or lift?')
    const by = typeof body.by?.user_uuid === 'string' ? body.by.user_uuid : ''
    if (!by) throw refuse(400, 'nobody behind it')
    const name = typeof body.name === 'string' ? body.name : ''
    const connection = await openDoc(hocuspocus, docName, { user: 'protect' })
    try {
        const doc = connection.document
        const elements = doc.getMap(ELEMENTS)
        const record = doc.getMap(PROTECTED)
        const named = new Set(ids)
        const withText = new Set(ids)
        elements.forEach((v, id) => {
            if (v?.containerId && named.has(v.containerId)) withText.add(id)
        })
        let changed = 0
        const at = Date.now()
        doc.transact(() => {
            for (const id of withText) {
                const v = elements.get(id)
                if (record.has(id) === body.protect) continue
                if (body.protect) {
                    if (!v || v.isDeleted === true) continue
                    record.set(id, { by, name, at })
                } else {
                    record.delete(id)
                }
                if (v) elements.set(id, newer({ ...v, locked: body.protect }))
                if (named.has(id)) changed++
            }
        }, PROTECT_ORIGIN)
        return { changed }
    } finally {
        await connection.disconnect()
    }
}
