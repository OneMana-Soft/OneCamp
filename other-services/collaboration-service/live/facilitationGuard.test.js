import { test, after } from 'node:test'
import assert from 'node:assert/strict'
import { FakeBackend, FakeRedis, TestClient, onClose, startServer, until } from '../testkit.js'
import { createLiveServer } from './server.js'

// V-462: a board's facilitation (its timer, its voting round and its private
// round) is its members' to run. A call's guest whom the call lets draw holds
// a writable connection, so a crafted guest client could start or end a round,
// and ending a private round reveals the held notes early. The relay puts back
// every guest connection's write to the facilitation map, failing closed;
// members' writes stand. Viewers' connections are read-only already.

const rejections = []
const onRejection = (r) => rejections.push(r)
process.on('unhandledRejection', onRejection)
after(() => process.off('unhandledRejection', onRejection))

const ID = '5a6b7c8d-9e0f-4a1b-8c2d-3e4f5a6b7c8d'
const NAME = `board:${ID}`
const SECRET = 'test-secret'
const GUEST = '{"guest":true,"sub":"guest-g-1","name":"Guest: Gil","call":"s-1"}'

async function world() {
    const backend = new FakeBackend()
    backend.boards.set(ID, { board_state: '' })
    backend.levels.set('guest-g-1', 'edit') // "Guests can draw" is on
    const collab = await startServer({ redis: new FakeRedis(), backend })
    const live = createLiveServer({ getSecret: () => SECRET, hocuspocus: collab.server.hocuspocus, deps: collab.live })
    await new Promise((resolve) => live.listen(0, '127.0.0.1', resolve))
    const stopLive = onClose(() => new Promise((resolve) => {
        live.close(resolve)
        live.closeAllConnections()
    }))
    const base = `http://127.0.0.1:${live.address().port}`
    const op = async (name, payload) => {
        const res = await fetch(`${base}/live/boards/${ID}/${name}`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json', 'X-Internal-Secret': SECRET },
            body: JSON.stringify(payload),
        })
        return { status: res.status, ...(await res.json()) }
    }
    const tabs = []
    const open = async (token) => {
        const tab = new TestClient({ url: collab.url, name: NAME, token })
        assert.equal(await tab.connect(), 'synced')
        tabs.push(tab)
        return tab
    }
    const doc = () => collab.server.hocuspocus.documents.get(NAME)
    const close = async () => {
        for (const tab of tabs) {
            await tab.close()
            tab.destroy()
        }
        await stopLive()
        await collab.stop()
        assert.deepEqual(rejections, [])
    }
    return { op, open, doc, close }
}

const fac = (tab) => tab.doc.getMap('facilitation')

test("a call guest's crafted client neither ends a private round nor starts or rewrites one", async () => {
    const w = await world()
    const sam = await w.open('{"sub":"sam"}')
    const guest = await w.open(GUEST)
    const round = { id: 'r-1', byId: 'sam', byName: 'Sam', startedAt: 1 }
    fac(sam).set('private', round)
    fac(sam).set('voting', { id: 'v-1', votes: 3 })
    const served = () => w.doc()?.getMap('facilitation')
    await until(() => served()?.get('private')?.id === 'r-1' && served()?.has('voting'), 2000, "Sam's rounds")
    await until(() => fac(guest).get('private')?.id === 'r-1', 2000, "the round in the guest's tab")

    // The guest ends the private round, rewrites the vote and starts a timer.
    fac(guest).delete('private')
    fac(guest).set('voting', { id: 'v-forged', votes: 99 })
    fac(guest).set('timer', { endsAt: 1 })
    fac(sam).set('check', 1)
    await until(() => served()?.get('check') === 1, 2000, 'a later change')
    await until(() => served()?.get('private')?.id === 'r-1' && served()?.get('voting')?.id === 'v-1' && !served()?.has('timer'), 2000, 'the facilitation put back')
    // Every tab, the guest's own included, holds the round as Sam left it.
    for (const tab of [sam, guest]) {
        await until(() => fac(tab).get('private')?.id === 'r-1' && fac(tab).get('voting')?.id === 'v-1' && !fac(tab).has('timer'), 2000, 'every tab put back')
    }
    await w.close()
})

test("members run the board's facilitation: start, change and end", async () => {
    const w = await world()
    const sam = await w.open('{"sub":"sam"}')
    const ed = await w.open('{"sub":"ed"}')
    await w.open(GUEST)
    fac(sam).set('private', { id: 'r-1', byId: 'sam', byName: 'Sam', startedAt: 1 })
    const served = () => w.doc()?.getMap('facilitation')
    await until(() => served()?.get('private')?.id === 'r-1', 2000, "Sam's round")
    fac(ed).set('timer', { endsAt: 5 })
    fac(ed).delete('private')
    await until(() => served()?.get('timer')?.endsAt === 5 && !served()?.has('private'), 2000, "Ed's changes")
    await w.close()
})
