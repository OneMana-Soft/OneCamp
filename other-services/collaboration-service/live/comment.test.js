import { test, after } from 'node:test'
import assert from 'node:assert/strict'
import { FakeBackend, FakeRedis, TestClient, onClose, sleep, startServer, until } from '../testkit.js'
import { createLiveServer } from './server.js'
import { comment, COMMENTS, MAX_COMMENT_TEXT, MAX_THREAD } from './ops/comment.js'

// Can comment on a board (miro-32 head B): a commenter's live canvas stays
// read-only (V-230, fail closed), so the API writes their comment through
// the comment op, which makes the caller the author whatever it is sent and
// refuses deleting anyone else's message.

const rejections = []
const onRejection = (r) => rejections.push(r)
process.on('unhandledRejection', onRejection)
after(() => process.off('unhandledRejection', onRejection))

const ID = '3c9e1d2a-5b4f-4a6e-9d8c-7b6a5f4e3d2c'
const NAME = `board:${ID}`
const SECRET = 'test-secret'
const CAM = { user_uuid: 'u-cam' }
const EDA = { user_uuid: 'u-eda' }

async function world() {
    const backend = new FakeBackend()
    backend.boards.set(ID, { board_state: '' })
    backend.levels.set('cam', 'comment')
    const collab = await startServer({ redis: new FakeRedis(), backend })
    const live = createLiveServer({ getSecret: () => SECRET, hocuspocus: collab.server.hocuspocus, deps: collab.live })
    await new Promise((resolve) => live.listen(0, '127.0.0.1', resolve))
    const stopLive = onClose(() => new Promise((resolve) => {
        live.close(resolve)
        live.closeAllConnections()
    }))
    const base = `http://127.0.0.1:${live.address().port}`
    const call = async (path, payload) => {
        const res = await fetch(`${base}${path}`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json', 'X-Internal-Secret': SECRET },
            body: JSON.stringify(payload),
        })
        return { status: res.status, ...(await res.json()) }
    }
    const op = (payload) => call(`/live/boards/${ID}/comment`, payload)
    const tabs = []
    const open = async (token) => {
        const tab = new TestClient({ url: collab.url, name: NAME, token })
        assert.equal(await tab.connect(), 'synced')
        tabs.push(tab)
        return tab
    }
    const pins = () => collab.server.hocuspocus.documents.get(NAME)?.getMap(COMMENTS)
    const close = async () => {
        for (const tab of tabs) {
            await tab.close()
            tab.destroy()
        }
        await stopLive()
        await collab.stop()
        assert.deepEqual(rejections, [])
    }
    return { op, call, open, pins, close, collab }
}

const add = (w, by, text = 'Ship the beta first', more = {}) => w.op({ action: 'add', x: 120.5, y: -40, text, by, name: by === CAM ? 'Cam' : 'Eda', ...more })

test("a pin is the caller's, whatever the request says, in the shape the board's tabs read", async () => {
    const w = await world()
    try {
        const tab = await w.open('{"sub":"editor"}')
        const res = await add(w, CAM, 'Ship the beta first', { authorId: 'u-mallory', authorName: 'Mallory', author_id: 'u-mallory', messages: [{ authorId: 'u-mallory', body: 'forged' }] })
        assert.equal(res.status, 200, res.msg)
        const c = res.data.comment
        assert.equal(res.data.removed, false)
        assert.equal(typeof c.id, 'string')
        assert.deepEqual({ x: c.x, y: c.y, resolved: c.resolved, authorId: c.authorId, authorName: c.authorName }, { x: 120.5, y: -40, resolved: false, authorId: 'u-cam', authorName: 'Cam' })
        assert.equal(c.messages.length, 1)
        assert.deepEqual({ authorId: c.messages[0].authorId, authorName: c.messages[0].authorName, body: c.messages[0].body }, { authorId: 'u-cam', authorName: 'Cam', body: 'Ship the beta first' })
        assert.equal(typeof c.createdAt, 'number')
        assert.equal(typeof c.messages[0].id, 'string')
        await until(() => tab.doc.getMap(COMMENTS).has(c.id), 2000, "the editor's tab sees the pin")
        assert.deepEqual(tab.doc.getMap(COMMENTS).get(c.id), c)

        const reply = await w.op({ action: 'reply', comment_id: c.id, text: 'and then the docs', by: EDA, name: 'Eda', authorId: 'u-cam' })
        assert.equal(reply.status, 200, reply.msg)
        assert.deepEqual(reply.data.comment.messages.map((m) => [m.authorId, m.body]), [['u-cam', 'Ship the beta first'], ['u-eda', 'and then the docs']])
        assert.equal(reply.data.comment.authorId, 'u-cam', 'a reply leaves the pin its author')
    } finally {
        await w.close()
    }
})

test("deleting is one's own messages only; a pin goes with its last message", async () => {
    const w = await world()
    try {
        // An editor's tab keeps the board loaded, so its comments are read where the op wrote them.
        await w.open('{"sub":"editor"}')
        const c = (await add(w, CAM)).data.comment
        const withReply = (await w.op({ action: 'reply', comment_id: c.id, text: 'mine', by: EDA, name: 'Eda' })).data.comment
        const [camMsg, edaMsg] = withReply.messages

        for (const payload of [{ message_id: edaMsg.id }, {}]) {
            const res = await w.op({ action: 'delete', comment_id: c.id, by: CAM, name: 'Cam', ...payload })
            assert.equal(res.status, 403, `deleting ${payload.message_id ? "another's message" : 'a pin with their reply'}`)
            assert.equal(w.pins().get(c.id).messages.length, 2, 'nothing changed')
        }
        const own = await w.op({ action: 'delete', comment_id: c.id, message_id: camMsg.id, by: CAM, name: 'Cam' })
        assert.equal(own.status, 200, own.msg)
        assert.deepEqual(own.data.comment.messages.map((m) => m.id), [edaMsg.id])
        assert.equal(own.data.removed, false)
        const last = await w.op({ action: 'delete', comment_id: c.id, message_id: edaMsg.id, by: EDA, name: 'Eda' })
        assert.deepEqual([last.status, last.data.removed, last.data.comment], [200, true, null])
        assert.equal(w.pins().has(c.id), false)

        const mine = (await add(w, CAM, 'just me')).data.comment
        const whole = await w.op({ action: 'delete', comment_id: mine.id, by: CAM, name: 'Cam' })
        assert.deepEqual([whole.status, whole.data.removed], [200, true])
        for (const payload of [{ action: 'reply', comment_id: 'gone', text: 'hi' }, { action: 'delete', comment_id: 'gone' }, { action: 'delete', comment_id: c.id, message_id: 'gone' }]) {
            assert.equal((await w.op({ ...payload, by: CAM, name: 'Cam' })).status, 404, JSON.stringify(payload))
        }
    } finally {
        await w.close()
    }
})

test('a reply to a full thread is refused as a conflict, and writes nothing', async () => {
    const w = await world()
    try {
        await w.open('{"sub":"editor"}')
        const c = (await add(w, CAM)).data.comment
        const full = { ...c, messages: Array.from({ length: MAX_THREAD }, (_, i) => ({ id: `m-${i}`, authorId: 'u-cam', authorName: 'Cam', body: 'x', createdAt: 1 })) }
        w.collab.server.hocuspocus.documents.get(NAME).getMap(COMMENTS).set(c.id, full)
        const res = await w.op({ action: 'reply', comment_id: c.id, text: 'one more', by: EDA, name: 'Eda' })
        assert.equal(res.status, 409, res.msg)
        assert.equal(w.pins().get(c.id).messages.length, MAX_THREAD)
    } finally {
        await w.close()
    }
})

test('a request the op cannot take writes nothing', async () => {
    const w = await world()
    try {
        await w.open('{"sub":"editor"}')
        for (const payload of [
            { action: 'add', x: 1, y: 1, text: 'hi', name: 'Nobody' },
            { action: 'paint', x: 1, y: 1, text: 'hi', by: CAM },
            { action: 'add', x: 1, text: 'hi', by: CAM },
            { action: 'add', x: 1, y: 'up', text: 'hi', by: CAM },
            { action: 'add', x: 1e8, y: 1, text: 'hi', by: CAM },
            { action: 'add', x: 1, y: 1, text: '   ', by: CAM },
            { action: 'add', x: 1, y: 1, text: 'a'.repeat(MAX_COMMENT_TEXT + 1), by: CAM },
            { action: 'reply', text: 'hi', by: CAM },
        ]) {
            assert.equal((await w.op(payload)).status, 400, JSON.stringify(payload).slice(0, 80))
        }
        assert.equal(w.pins().size, 0)
    } finally {
        await w.close()
    }
})

test("the op is a board's only: a doc has none, and a doc's name is refused", async () => {
    const w = await world()
    try {
        assert.equal((await w.call(`/live/docs/${ID}/comment`, { action: 'add', x: 1, y: 1, text: 'hi', by: CAM })).status, 404)
        await assert.rejects(comment({ docName: ID, body: { action: 'add', x: 1, y: 1, text: 'hi', by: CAM }, hocuspocus: w.collab.server.hocuspocus }), (err) => err.status === 400)
        assert.equal(w.collab.server.hocuspocus.documents.has(ID), false, 'no doc opened')
    } finally {
        await w.close()
    }
})

test("a commenter's own tab stays read-only, though the API says they may comment (V-230, fail closed)", async () => {
    const w = await world()
    try {
        const editor = await w.open('{"sub":"editor"}')
        const cam = await w.open('{"sub":"cam"}')
        assert.equal(cam.scope, 'readonly')
        cam.doc.getMap(COMMENTS).set('forged', { id: 'forged', x: 0, y: 0, resolved: false, createdAt: 1, authorId: 'u-eda', authorName: 'Eda', messages: [] })
        cam.doc.getMap('elements').set('drawn', { id: 'drawn', type: 'rectangle', version: 1, x: 0 })
        await until(() => cam.statuses.includes(false), 2000, "the commenter's refusal")
        await sleep(100)
        assert.equal(w.pins().has('forged'), false, 'the board has no pin from their tab')
        assert.equal(editor.doc.getMap(COMMENTS).has('forged'), false, "the editor's tab neither")
        assert.equal(editor.doc.getMap('elements').has('drawn'), false, 'nor their drawing')
    } finally {
        await w.close()
    }
})
