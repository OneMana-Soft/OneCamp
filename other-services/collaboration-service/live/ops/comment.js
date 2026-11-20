// Comments on a board, written by the API (miro-32 Can comment).
//
// A board's comments are one shared map, "comments": a pin's id -> { id, x,
// y, resolved, createdAt, authorId, authorName, messages: [{ id, authorId,
// authorName, body, createdAt }] } (web components/board/boardComments.tsx).
// Editors write it from their tabs. Everyone else's tab is read-only
// (collabServer.js, V-230, failing closed), so a commenter's comment comes
// through this op, once the API has checked that the person may comment
// (business/Board CommentOnBoard): it writes as body.by.user_uuid, named
// body.name, whatever else the body says, and deletes only that person's
// own messages. It names its writer as authorGuard.js names a socket's
// (writerOf), and the API names them from the source a socket's name comes
// from (userDomain.NameForGuests, as POST /boardColab/authorize answers), so
// a commenter's pin reads as an editor's does.

import { randomUUID } from 'node:crypto'
import { BOARD_PREFIX } from '../../collabServer.js'
import { COMMENTS, writerOf } from './authorGuard.js'
import { openDoc } from './write.js'

export { COMMENTS }

// The origin of this service's own writes to a board's comments.
export const COMMENT_ORIGIN = Object.freeze({ comment: true })

// The most characters one message holds (the API's route says the same), the
// most messages one pin's thread holds, and how far from the board's middle a
// pin may sit.
export const MAX_COMMENT_TEXT = 4000
export const MAX_THREAD = 500
const MAX_COORD = 1e7

const refuse = (status, msg) => Object.assign(new Error(msg), { status })
const isId = (v) => typeof v === 'string' && v !== '' && v.length <= 64
const isCoord = (v) => typeof v === 'number' && Number.isFinite(v) && Math.abs(v) <= MAX_COORD

// comment adds a pin at body.x, body.y (action "add"), replies on
// body.comment_id ("reply"), or deletes body.message_id from it ("delete";
// the whole pin when no message is named, only if every message in it is
// the person's). A pin goes with its last message. Answers the pin as it now
// is, or { comment: null, removed: true }. 404 for a pin or message the
// board doesn't hold, 403 for someone else's message, 409 for a reply to a
// full thread.
export async function comment({ docName, body, hocuspocus }) {
    if (typeof docName !== 'string' || !docName.startsWith(BOARD_PREFIX)) throw refuse(400, 'comments are on boards only')
    // Someone the API names nobody is 'Anonymous', as on a socket (collabServer.js).
    const who = writerOf({ user: { id: body?.by?.user_uuid, name: typeof body?.name === 'string' && body.name ? body.name : 'Anonymous' } })
    if (!isId(who.id)) throw refuse(400, 'nobody behind it')
    const { id: by, name } = who
    const action = body.action
    const text = typeof body.text === 'string' ? body.text.trim() : ''
    const textOK = text !== '' && [...text].length <= MAX_COMMENT_TEXT
    if (action === 'add' && !(textOK && isCoord(body.x) && isCoord(body.y))) throw refuse(400, 'a pin needs a spot and its text')
    if (action === 'reply' && !(textOK && isId(body.comment_id))) throw refuse(400, 'a reply needs its pin and its text')
    if (action === 'delete' && !(isId(body.comment_id) && (body.message_id === undefined || isId(body.message_id)))) throw refuse(400, 'name the pin')
    if (!['add', 'reply', 'delete'].includes(action)) throw refuse(400, 'add, reply or delete?')

    const connection = await openDoc(hocuspocus, docName, { user: 'comment' })
    try {
        const pins = connection.document.getMap(COMMENTS)
        const now = Date.now()
        const message = () => ({ id: randomUUID(), authorId: by, authorName: name, body: text, createdAt: now })
        let next = null
        if (action === 'add') {
            const id = randomUUID()
            next = { id, x: body.x, y: body.y, resolved: false, createdAt: now, authorId: by, authorName: name, messages: [message()] }
        } else {
            const cur = pins.get(body.comment_id)
            if (!cur) throw refuse(404, 'no such comment')
            const messages = Array.isArray(cur.messages) ? cur.messages : []
            if (action === 'reply') {
                if (messages.length >= MAX_THREAD) throw refuse(409, `the thread is full: at most ${MAX_THREAD} messages on one comment`)
                next = { ...cur, messages: [...messages, message()] }
            } else if (body.message_id !== undefined) {
                const m = messages.find((x) => x?.id === body.message_id)
                if (!m) throw refuse(404, 'no such message')
                if (m.authorId !== by) throw refuse(403, 'only your own messages')
                const kept = messages.filter((x) => x !== m)
                next = kept.length > 0 ? { ...cur, messages: kept } : null
            } else {
                if (cur.authorId !== by || messages.some((x) => x?.authorId !== by)) throw refuse(403, 'only your own messages')
            }
        }
        const id = next?.id ?? body.comment_id
        connection.document.transact(() => {
            if (next) pins.set(id, next)
            else pins.delete(id)
        }, COMMENT_ORIGIN)
        return { comment: next, removed: next === null }
    } finally {
        await connection.disconnect()
    }
}
