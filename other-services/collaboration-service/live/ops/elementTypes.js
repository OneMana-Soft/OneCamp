// The element types a board draws (V-547), and the guard that holds a board to
// them. The web holds the same list wherever an element comes onto a board
// (lib/board/elementTypes.ts, whose test holds it to Excalidraw's own types); a
// type is added in both, the web's first.
//
// Left out: Excalidraw's "iframe", which it draws as a live page made from the
// element's own HTML (customData.generationData), with scripts, forms and
// popups that leave its sandbox, and "magicframe", the frame that makes one.
// We ship neither tool, so only a crafted client writes them, and to every
// viewer and guest one would be a page of its writer's choosing: a sign-in
// form that isn't ours. ("selection" is Excalidraw's marquee, never an element
// of a scene.)
//
// THE GUARD. An element of any other type, or of none, is taken out of a board
// as it opens (a board stored before this guard, or drawn by the API), and
// from every change after it, whoever made it: a socket, an op of this
// service's, another instance. One a change turned from a type the board draws
// goes back as it was, as a newer version every tab takes (the writer's
// included); any other is deleted. A change is repaired, never dropped
// (mapGuard.js).

import { newer } from './mapGuard.js'

export const BOARD_ELEMENT_TYPES = new Set([
    'rectangle',
    'diamond',
    'ellipse',
    'arrow',
    'line',
    'freedraw',
    'text',
    'image',
    'frame',
    'embeddable',
])

// The origin of this guard's own changes.
export const ELEMENT_TYPES_ORIGIN = Object.freeze({ elementTypes: true })

const ELEMENTS = 'elements'

// isBoardElement is whether v is an element a board draws: an object whose
// type is on the list.
export function isBoardElement(v) {
    return typeof v === 'object' && v !== null && BOARD_ELEMENT_TYPES.has(v.type)
}

// guardElementTypes holds a board's elements to the types it draws, from now
// on. Returns how many it took out of the board as it was.
export function guardElementTypes(doc) {
    const elements = doc.getMap(ELEMENTS)
    const stored = []
    elements.forEach((v, id) => {
        if (!isBoardElement(v)) stored.push(id)
    })
    if (stored.length > 0) {
        doc.transact(() => {
            for (const id of stored) elements.delete(id)
        }, ELEMENT_TYPES_ORIGIN)
    }
    elements.observe((event) => {
        if (event.transaction.origin === ELEMENT_TYPES_ORIGIN) return
        const putBack = []
        const out = []
        event.changes.keys.forEach((change, id) => {
            if (change.action === 'delete') return
            const now = elements.get(id)
            if (isBoardElement(now)) return
            if (isBoardElement(change.oldValue)) putBack.push([id, newer(change.oldValue, now)])
            else out.push(id)
        })
        if (putBack.length === 0 && out.length === 0) return
        doc.transact(() => {
            for (const [id, v] of putBack) elements.set(id, v)
            for (const id of out) elements.delete(id)
        }, ELEMENT_TYPES_ORIGIN)
    })
    return stored.length
}
