// A call's guests drawing on the board on its stage (calls package B,
// PROGRAM.md "Guest strokes", approved 26 Oct).
//
// WHO DREW WHAT. Elements are whole objects in one shared map, merged by their
// `version`, so a mark written inside an element would race the guest's next
// edit. This service keeps its own map beside them, guestAuthors: an element's
// id -> { id, name, at }, the call guest who changed it last. Only this
// service writes it (trackGuestStrokes): a guest's change records them, a
// member's change takes the record off (the element is a member's work now),
// and a change to the map from any socket is put back as it was, so nobody
// forges or erases who drew what.
//
// UNDO. "Guests can draw" keeps the board as it was (guestsSnapshot; the API
// stores it as a board version with the reason 'guests', once per time on a
// stage), and once it is stored the record starts afresh (guestsBegin). "Undo guests'
// drawing" (undoGuests) hands that version back: every element in
// guestAuthors returns as the version had it, or goes if guests drew it, in
// one transaction, as a newer version so every tab takes it. What members did
// since stays.

import * as Y from 'yjs'
import { openDoc } from './write.js'

export const GUEST_AUTHORS = 'guestAuthors'

// The origin of this service's own changes to a board's elements and record.
export const GUEST_ORIGIN = Object.freeze({ guestStrokes: true })

const ELEMENTS = 'elements'

// trackGuestStrokes keeps a board's guestAuthors as its sockets change it.
// Changes with no socket behind them (loading, this service's ops, pruning)
// are nobody's.
export function trackGuestStrokes(doc) {
    const elements = doc.getMap(ELEMENTS)
    const authors = doc.getMap(GUEST_AUTHORS)
    elements.observe((event) => {
        const context = event.transaction.origin?.context
        if (!context?.user) return
        doc.transact(() => {
            for (const id of event.keysChanged) {
                if (context.guest === true) {
                    authors.set(id, { id: context.user.id, name: context.user.name, at: Date.now() })
                } else if (authors.has(id)) {
                    authors.delete(id)
                }
            }
        }, GUEST_ORIGIN)
    })
    authors.observe((event) => {
        if (!event.transaction.origin?.context) return
        doc.transact(() => {
            event.changes.keys.forEach((change, id) => {
                if (change.action === 'add') authors.delete(id)
                else authors.set(id, change.oldValue)
            })
        }, GUEST_ORIGIN)
    })
}

const refuse = (status, msg) => Object.assign(new Error(msg), { status })

// withBoard runs fn on the board's live copy, opened as this service.
async function withBoard(hocuspocus, docName, fn) {
    const connection = await openDoc(hocuspocus, docName, { user: 'guest-strokes' })
    try {
        return fn(connection.document)
    } finally {
        await connection.disconnect()
    }
}

// guestsSnapshot answers the board as it is now ({ state, elements }: its Yjs
// state, base64, and how many elements show): the version an undo goes back
// to. It changes nothing.
export async function guestsSnapshot({ docName, hocuspocus }) {
    return withBoard(hocuspocus, docName, (doc) => {
        let elements = 0
        doc.getMap(ELEMENTS).forEach((v) => {
            if (v && v.isDeleted !== true) elements++
        })
        return { state: Buffer.from(Y.encodeStateAsUpdate(doc)).toString('base64'), elements }
    })
}

// guestsBegin starts guestAuthors afresh, once the version guestsSnapshot
// answered is stored.
export async function guestsBegin({ docName, hocuspocus }) {
    return withBoard(hocuspocus, docName, (doc) => {
        doc.transact(() => doc.getMap(GUEST_AUTHORS).clear(), GUEST_ORIGIN)
        return {}
    })
}

// undoGuests takes back what guests did since the version body.state (from
// guestsSnapshot): each element they changed last returns as it was there, or
// goes if they drew it. Answers how many it took back.
export async function undoGuests({ docName, body, hocuspocus }) {
    const state = typeof body?.state === 'string' ? body.state : ''
    if (!state) throw refuse(400, 'no version to go back to')
    const before = new Y.Doc()
    try {
        Y.applyUpdate(before, Buffer.from(state, 'base64'))
    } catch {
        throw refuse(400, 'that version could not be read')
    }
    const was = before.getMap(ELEMENTS)
    return withBoard(hocuspocus, docName, (doc) => {
        const elements = doc.getMap(ELEMENTS)
        const authors = doc.getMap(GUEST_AUTHORS)
        let undone = 0
        const now = Date.now()
        doc.transact(() => {
            for (const id of [...authors.keys()]) {
                const current = elements.get(id)
                const prior = was.get(id)
                authors.delete(id)
                if (!current) continue
                const version = Math.max(current.version || 0, prior?.version || 0) + 1
                const versionNonce = Math.floor(Math.random() * 2 ** 31)
                elements.set(id, prior
                    ? { ...prior, version, versionNonce, updated: now }
                    : { ...current, isDeleted: true, version, versionNonce, updated: now })
                undone++
            }
        }, GUEST_ORIGIN)
        return { undone }
    })
}
