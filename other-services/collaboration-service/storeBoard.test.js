// Saving a board (storeBoard.js): what the backend's safety net reads of it.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import * as Y from 'yjs'
import { persistBoard, TOMBSTONE_PRUNE_AGE_MS } from './storeBoard.js'

const el = (id, extra = {}) => ({ id, type: 'rectangle', version: 2, isDeleted: false, updated: 1_000, ...extra })

function board(elements) {
    const doc = new Y.Doc()
    doc.transact(() => {
        const m = doc.getMap('elements')
        for (const e of elements) m.set(e.id, e)
    })
    return doc
}

async function save(doc, now = 2_000) {
    const posts = []
    const outcome = await persistBoard({ id: 'b1', document: doc, post: async (p) => posts.push(p), log: { log() {}, error() {} }, now: () => now })
    return { outcome, post: posts[0] }
}

// The backend keeps a version from before a sharp drop in what a person sees
// (business/Board/boardSnapshot.go, W-0b). A deleted element stays in the
// document, marked, for ten minutes: an undo that took everything back left
// the count where it was, so nothing was kept.
test('a board says how many elements a person sees, not counting deleted ones', async () => {
    const { post } = await save(board([el('a'), el('b'), el('c'), el('x', { isDeleted: true }), el('y', { isDeleted: true })]))
    assert.equal(post.snippet, '3 elements')
})

test('an emptied board says none, while its deleted elements wait to be pruned', async () => {
    const { outcome, post } = await save(board([el('a', { isDeleted: true }), el('b', { isDeleted: true })]))
    assert.equal(outcome, 'saved')
    assert.equal(post.snippet, '0 elements')
})

test('deleted elements older than the window are pruned on a save, and the count is of what is left', async () => {
    const doc = board([el('a'), el('old', { isDeleted: true, updated: 0 }), el('new', { isDeleted: true, updated: 1_000 })])
    const { post } = await save(doc, TOMBSTONE_PRUNE_AGE_MS + 1)
    assert.deepEqual([...doc.getMap('elements').keys()].sort(), ['a', 'new'])
    assert.equal(post.snippet, '1 elements')
})

test('a brand-new empty board is not saved', async () => {
    const { outcome, post } = await save(new Y.Doc())
    assert.equal(outcome, 'skipped')
    assert.equal(post, undefined)
})
