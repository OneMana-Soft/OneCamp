import { test } from 'node:test'
import assert from 'node:assert/strict'
import * as Y from 'yjs'
import { COLLAB_EXTENSIONS } from '../../extensions.js'
import { TaskItemLinkSchema } from '../../schema/connected.js'
import { FakeBackend, FakeRedis, TestClient, onClose, startServer, until } from '../../testkit.js'
import { createLiveServer } from '../server.js'
import { checksOf, setChecks, setTaskChecks } from './taskChecks.js'

const A = '11111111-1111-4111-8111-111111111111'
const B = '22222222-2222-4222-8222-222222222222'

// A body with a to-do list, one item nested under another, as the editor writes it.
function body() {
    const doc = new Y.Doc()
    const fragment = doc.getXmlFragment('default')
    const item = (taskId, checked) => {
        const li = new Y.XmlElement('taskItem')
        li.setAttribute('checked', checked)
        if (taskId) li.setAttribute('taskId', taskId)
        return li
    }
    const outer = item(A, false)
    const inner = new Y.XmlElement('taskList')
    const nested = item(B, true)
    inner.insert(0, [nested])
    outer.insert(0, [inner])
    const list = new Y.XmlElement('taskList')
    list.insert(0, [outer, item(null, false)])
    fragment.insert(0, [list])
    return { doc, fragment, outer, nested }
}

test('a task done elsewhere ticks its to-do, nested or not, and leaves the rest', () => {
    const { doc, fragment, outer, nested } = body()
    let n = null
    doc.transact(() => { n = setChecks(fragment, new Map([[A, true], [B, false]])) })
    assert.deepEqual(n, { changed: 2, held: 2 })
    assert.equal(outer.getAttribute('checked'), true)
    assert.equal(nested.getAttribute('checked'), false)
})

test('boxes that already say so are not written again', () => {
    const { doc, fragment } = body()
    let updates = 0
    doc.on('update', () => updates++)
    let n = null
    doc.transact(() => { n = setChecks(fragment, new Map([[A, false], [B, true]])) })
    assert.deepEqual(n, { changed: 0, held: 2 })
    assert.equal(updates, 0)
})

test('a doc whose to-do was deleted says it holds none, so the API stops following the task', () => {
    const { fragment } = body()
    assert.deepEqual(setChecks(fragment, new Map([['33333333-3333-4333-8333-333333333333', true]])), { changed: 0, held: 0 })
})

test('a request must name tasks by id, with true or false', () => {
    assert.deepEqual([...checksOf({ task_checks: { [A.toUpperCase()]: true } })], [[A, true]])
    for (const bad of [undefined, {}, { task_checks: {} }, { task_checks: { nope: true } }, { task_checks: { [A]: 'yes' } }, { task_checks: [A] }]) {
        assert.throws(() => checksOf(bad), (e) => e.status === 400)
    }
})

// Over a real collaboration server: the task done elsewhere ticks its to-do in
// a tab that has the doc open, as one transaction credited to whoever did it,
// and the doc is stored with the box ticked.
test('a tab with the doc open sees its to-do ticked, once, and the doc is stored ticked', async () => {
    const ID = '5e1d9b7e-2a4f-4e8b-9c6d-5f7a8b9c0d1e'
    const SECRET = 'test-secret'
    const backend = new FakeBackend()
    backend.doc(ID, `<ul data-type="taskList"><li data-type="taskItem" data-checked="false" data-task-id="${A}"><p>Write the post</p></li></ul>`)
    const extensions = [...COLLAB_EXTENSIONS.filter((e) => e.name !== TaskItemLinkSchema.name), TaskItemLinkSchema]
    const collab = await startServer({ redis: new FakeRedis(), backend, extensions })
    const live = createLiveServer({ getSecret: () => SECRET, routes: new Map([['setTaskChecks', setTaskChecks]]), hocuspocus: collab.server.hocuspocus, deps: collab.live })
    await new Promise((resolve) => live.listen(0, '127.0.0.1', resolve))
    const stopLive = onClose(() => new Promise((resolve) => {
        live.close(resolve)
        live.closeAllConnections()
    }))
    const tab = new TestClient({ url: collab.url, name: ID, token: '{"sub":"user-1"}' })
    onClose(async () => {
        await tab.close()
        tab.destroy()
    })
    assert.equal(await tab.connect(), 'synced')
    const doc = collab.server.hocuspocus.documents.get(ID)
    let updates = 0
    doc.on('update', () => updates++)
    const res = await fetch(`http://127.0.0.1:${live.address().port}/live/docs/${ID}/setTaskChecks`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-Internal-Secret': SECRET },
        body: JSON.stringify({ task_checks: { [A]: true }, by: { user_uuid: 'user-9' } }),
    })
    const out = await res.json()
    assert.equal(res.status, 200, out.msg)
    assert.equal(out.data.changed, 1)
    assert.equal(out.data.held, 1)
    assert.equal(updates, 1, 'one transaction')
    const item = () => tab.doc.getXmlFragment('default').get(0).get(0)
    await until(() => item().getAttribute('checked') === true, 2000, 'the tab sees the tick')
    await until(() => /data-checked="true"/.test(backend.docs.get(ID).doc_body), 2000, 'the doc is stored ticked')
    await stopLive()
    await collab.stop()
})
