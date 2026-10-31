import { test } from 'node:test'
import assert from 'node:assert/strict'
import * as Y from 'yjs'
import { COLLAB_EXTENSIONS } from '../../extensions.js'
import { FakeBackend, FakeRedis, TestClient, onClose, startServer, until } from '../../testkit.js'
import { createLiveServer } from '../server.js'
import { BLOCK_ID_ATTR, elementsNamed, htmlToBlocks } from './blocks.js'
import { blocksOf, dropBlocks, placeSubpage, removeSubpage } from './subpages.js'

const PAGE = '7a1e0c52-4b8d-4f3e-9a6b-2c5d8e1f0a37'
const OTHER = '3c9b2d41-6e7f-4a8b-b1c2-d3e4f5a6b7c8'
const block = (id) => `<div data-type="subpage" data-doc-id="${id}"></div>`

// A body as the editor stores it, in a Y.Doc of its own.
function bodyOf(html) {
    const doc = new Y.Doc()
    const fragment = doc.getXmlFragment('default')
    fragment.insert(0, htmlToBlocks(html, COLLAB_EXTENSIONS).map((b) => b.node))
    return { doc, fragment }
}

const pagesIn = (node) => elementsNamed(node, 'subpage').map(({ node: n }) => n.getAttribute('docId'))
const names = (node) => node.toArray().map((n) => n.nodeName)
const emptyParagraph = () => htmlToBlocks('<p></p>', COLLAB_EXTENSIONS)[0].node

test("a page's blocks are found wherever the editor nests them, and no other page's", () => {
    const { fragment } = bodyOf(`<p>Plans</p>${block(PAGE)}<div data-type="callout">${block(PAGE)}</div>` +
        `<div data-type="column-list"><div data-type="column"><p>Left</p>${block(OTHER)}</div><div data-type="column">${block(PAGE)}<p>Right</p></div></div>`)
    assert.equal(blocksOf(fragment, PAGE).length, 3)
    assert.equal(blocksOf(fragment, OTHER).length, 1)
    assert.equal(blocksOf(fragment, '0f0f0f0f-0f0f-4f0f-8f0f-0f0f0f0f0f0f').length, 0)
})

test("taking a page's blocks out leaves the rest, and a container it would empty keeps an empty line", () => {
    const { doc, fragment } = bodyOf(`<p>Plans</p>${block(PAGE)}<div data-type="callout">${block(PAGE)}</div>` +
        `<div data-type="column-list"><div data-type="column"><p>Left</p>${block(OTHER)}</div><div data-type="column">${block(PAGE)}<p>Right</p></div></div>`)
    let removed = 0
    doc.transact(() => { removed = dropBlocks(blocksOf(fragment, PAGE), emptyParagraph) })
    assert.equal(removed, 3)
    assert.deepEqual(pagesIn(fragment), [OTHER])
    assert.deepEqual(names(fragment), ['paragraph', 'callout', 'columnList'])
    // The callout held only the page: without a line it reads as nothing, and
    // an editor drops it whole (y-prosemirror), so it keeps an empty one.
    assert.deepEqual(names(fragment.get(1)), ['paragraph'])
    const [left, right] = fragment.get(2).toArray()
    assert.deepEqual(names(left), ['paragraph', 'subpage'])
    assert.deepEqual(names(right), ['paragraph'])
    assert.equal(right.get(0).toString().includes('Right'), true)
})

test('a body that held only the page keeps an empty line, as a blank doc does', () => {
    const { doc, fragment } = bodyOf(block(PAGE))
    doc.transact(() => dropBlocks(blocksOf(fragment, PAGE), emptyParagraph))
    assert.deepEqual(names(fragment), ['paragraph'])
    assert.equal(fragment.get(0).length, 0)
})

// Over a real collaboration server, as the API calls the ops (business/Doc
// FollowMove): a tab with the doc open sees the change arrive as one
// transaction, and the doc is stored with it.
async function serve(html) {
    const ID = '5e1d9b7e-2a4f-4e8b-9c6d-5f7a8b9c0d1e'
    const SECRET = 'test-secret'
    const backend = new FakeBackend()
    backend.doc(ID, html)
    const collab = await startServer({ redis: new FakeRedis(), backend })
    const live = createLiveServer({
        getSecret: () => SECRET,
        routes: new Map([['removeSubpage', removeSubpage], ['placeSubpage', placeSubpage]]),
        hocuspocus: collab.server.hocuspocus,
        deps: collab.live,
    })
    await new Promise((resolve) => live.listen(0, '127.0.0.1', resolve))
    onClose(() => new Promise((resolve) => {
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
    const seen = { updates: 0 }
    doc.on('update', () => seen.updates++)
    const call = async (op, docId) => {
        const res = await fetch(`http://127.0.0.1:${live.address().port}/live/docs/${ID}/${op}`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json', 'X-Internal-Secret': SECRET },
            body: JSON.stringify({ doc_id: docId, by: { user_uuid: 'user-9' } }),
        })
        return { status: res.status, out: await res.json() }
    }
    return { ID, backend, tab, seen, call }
}

test("a moved page's block leaves its old parent in one transaction, and the doc is stored without it", async () => {
    const { ID, backend, tab, seen, call } = await serve(`<p>Plans</p>${block(PAGE)}<div data-type="callout">${block(PAGE)}</div>${block(OTHER)}`)
    const { status, out } = await call('removeSubpage', PAGE.toUpperCase())
    assert.equal(status, 200, out.msg)
    assert.equal(out.data.removed, 2)
    assert.equal(seen.updates, 1, 'one transaction')
    await until(() => pagesIn(tab.doc.getXmlFragment('default')).join() === OTHER, 2000, 'the tab sees both blocks go')
    await until(() => !backend.docs.get(ID).doc_body.includes(PAGE) && backend.docs.get(ID).doc_body.includes(OTHER), 2000, 'the doc is stored without them')
    const again = await call('removeSubpage', PAGE)
    assert.equal(again.status, 404, 'nothing left to take: refused, so nobody is credited with a change')
    assert.equal(seen.updates, 1)
})

test("a page's block lands at the end of its new parent, once", async () => {
    const { ID, backend, tab, seen, call } = await serve(`<p>Plans</p>${block(OTHER)}`)
    const { status, out } = await call('placeSubpage', PAGE)
    assert.equal(status, 200, out.msg)
    assert.equal(out.data.blocks, 1)
    assert.equal(out.data.block_ids.length, 1, 'the block takes an id where it lands')
    assert.equal(seen.updates, 1, 'one transaction')
    const body = () => tab.doc.getXmlFragment('default')
    await until(() => pagesIn(body()).join() === `${OTHER},${PAGE}`, 2000, 'the tab sees it arrive last')
    assert.equal(body().get(body().length - 1).getAttribute(BLOCK_ID_ATTR), out.data.block_ids[0])
    await until(() => backend.docs.get(ID).doc_body.includes(`data-doc-id="${PAGE}"`), 2000, 'the doc is stored with it')
    const again = await call('placeSubpage', PAGE)
    assert.equal(again.status, 409, 'a doc that holds the page already, as a link placed earlier or this block, takes no second one')
    assert.equal(seen.updates, 1)
})

test('the ops take a page by its id, and nothing else', async () => {
    const { call } = await serve(`<p>Plans</p>`)
    for (const op of ['removeSubpage', 'placeSubpage']) {
        for (const bad of [undefined, '', 'not-a-page', 42]) {
            assert.equal((await call(op, bad)).status, 400, `${op} ${bad}`)
        }
    }
})
