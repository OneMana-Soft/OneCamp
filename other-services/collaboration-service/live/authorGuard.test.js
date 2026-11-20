import { test, after } from 'node:test'
import assert from 'node:assert/strict'
import { FakeBackend, FakeRedis, TestClient, startServer, until } from '../testkit.js'

// V-965: whoever writes a board over the socket (its editors, a call's guest
// who may draw) writes as themselves. A comment, a reply, a reaction, a note's
// maker, a vote or a facilitation round naming someone else, or changing
// someone else's words, is put back as it was and logged (the document, the
// writer and the field; never the text); one's own stands as written, but for
// the name, which is the one the API gives the writer. A doc's record of where
// an agent wrote is this service's alone.

const rejections = []
const onRejection = (r) => rejections.push(r)
process.on('unhandledRejection', onRejection)
after(() => process.off('unhandledRejection', onRejection))

const ID = '7d1e0c3b-5a6f-4b8e-9c0d-1e2f3a4b5c6d'
const NAME = `board:${ID}`
const DOC = '2b4d6f80-1a3c-4e5f-8a9b-0c1d2e3f4a5b'
const GUEST = '{"guest":true,"sub":"guest-g-1","name":"Guest: Gil","call":"s-1"}'

async function world() {
    const backend = new FakeBackend()
    backend.boards.set(ID, { board_state: '' })
    backend.doc(DOC, '<p>Hello</p>')
    backend.names.set('sam', 'Sam Rivera')
    backend.names.set('ann', 'Ann Lee')
    backend.levels.set('guest-g-1', 'edit') // a call's guest who may draw
    const collab = await startServer({ redis: new FakeRedis(), backend })
    const tabs = []
    const open = async (token, name = NAME) => {
        const tab = new TestClient({ url: collab.url, name, token })
        assert.equal(await tab.connect(), 'synced')
        tabs.push(tab)
        return tab
    }
    const doc = (name = NAME) => collab.server.hocuspocus.documents.get(name)
    const map = (m, name = NAME) => doc(name).getMap(m)
    const lines = () => collab.logs.lines.filter((l) => l.includes('naming someone else'))
    const close = async () => {
        for (const tab of tabs) {
            await tab.close()
            tab.destroy()
        }
        await collab.stop()
        assert.deepEqual(rejections, [])
    }
    return { open, doc, map, lines, close }
}

const msg = (id, who, name, body, extra = {}) => ({ id, authorId: who, authorName: name, body, createdAt: 1, ...extra })
const pin = (id, who, name, messages) => ({ id, x: 10, y: 20, resolved: false, createdAt: 1, authorId: who, authorName: name, messages })
const set = (tab, m, key, value) => tab.doc.getMap(m).set(key, value)
// A marker written after a change, from the same tab: once the board has it,
// it has had the change before it, and whatever the change brought about.
let n = 0
const settle = async (w, tab, name = NAME) => {
    const key = `marker-${++n}`
    set(tab, 'markers', key, n)
    await until(() => w.map('markers', name).get(key) === n, 2000, 'the marker')
}

test("an editor's comment as someone else is put back and logged, without its text; their own stands", async () => {
    const w = await world()
    const sam = await w.open('{"sub":"sam"}')
    set(sam, 'comments', 'c-forged', pin('c-forged', 'ann', 'Ann Lee', [msg('m-1', 'ann', 'Ann Lee', 'approve the payment')]))
    set(sam, 'comments', 'c-own', pin('c-own', 'sam', 'Sam Rivera', [msg('m-2', 'sam', 'Sam Rivera', 'my own words')]))
    await settle(w, sam)
    assert.equal(w.map('comments').has('c-forged'), false, 'a pin as Ann is put back')
    assert.deepEqual(w.map('comments').get('c-own'), pin('c-own', 'sam', 'Sam Rivera', [msg('m-2', 'sam', 'Sam Rivera', 'my own words')]))
    await until(() => w.lines().length === 1, 2000, 'the log line')
    const [line] = w.lines()
    assert.ok(line.includes(NAME) && line.includes('sam') && line.includes('comments'), line)
    assert.ok(!line.includes('approve the payment'), 'no text in the log')
    // A message of one's own inside someone else's pin is someone else's pin all the same.
    set(sam, 'comments', 'c-mixed', pin('c-mixed', 'sam', 'Sam Rivera', [msg('m-3', 'sam', 'Sam Rivera', 'hi'), msg('m-4', 'ann', 'Ann Lee', 'agreed')]))
    await settle(w, sam)
    assert.equal(w.map('comments').has('c-mixed'), false, 'a thread with a message as Ann is put back')
    await w.close()
})

test("an editor can't reply or react as someone else, or rewrite their words; their own reply and reaction stand", async () => {
    const w = await world()
    const ann = await w.open('{"sub":"ann"}')
    const sam = await w.open('{"sub":"sam"}')
    const anns = pin('c-1', 'ann', 'Ann Lee', [msg('m-1', 'ann', 'Ann Lee', 'first')])
    set(ann, 'comments', 'c-1', anns)
    await until(() => sam.doc.getMap('comments').has('c-1'), 2000, "Ann's pin on Sam's tab")
    const tries = {
        'a reply as Ann': { ...anns, messages: [...anns.messages, msg('m-2', 'ann', 'Ann Lee', 'and approved')] },
        "Ann's words rewritten": { ...anns, messages: [msg('m-1', 'ann', 'Ann Lee', 'rejected')] },
        "Ann's name on her pin changed": { ...anns, authorName: 'Ann (fired)' },
        'a reaction as Ann': { ...anns, messages: [{ ...anns.messages[0], reactions: { '👍': ['ann'] } }] },
        'the pin taken over': { ...anns, authorId: 'sam', authorName: 'Sam Rivera' },
    }
    for (const [what, value] of Object.entries(tries)) {
        set(sam, 'comments', 'c-1', value)
        await settle(w, sam)
        assert.deepEqual(w.map('comments').get('c-1'), anns, `${what} is put back`)
    }
    assert.equal(w.lines().length, Object.keys(tries).length, 'each one logged')
    const replied = { ...anns, resolved: true, messages: [{ ...anns.messages[0], reactions: { '👍': ['sam'] } }, msg('m-3', 'sam', 'Sam Rivera', 'seen')] }
    set(sam, 'comments', 'c-1', replied)
    await settle(w, sam)
    assert.deepEqual(w.map('comments').get('c-1'), replied, 'his own reply and reaction, and resolving it, stand')
    // Deleting stays the editors' (it names nobody).
    sam.doc.getMap('comments').delete('c-1')
    await settle(w, sam)
    assert.equal(w.map('comments').has('c-1'), false)
    await w.close()
})

test("an own comment carries the name the API gives its writer, whatever the tab says", async () => {
    const w = await world()
    const sam = await w.open('{"sub":"sam"}')
    set(sam, 'comments', 'c-1', pin('c-1', 'sam', 'Ann Lee', [msg('m-1', 'sam', 'Ann Lee', 'hello')]))
    await until(() => w.map('comments').get('c-1')?.authorName === 'Sam Rivera', 2000, 'the stamped name')
    assert.deepEqual(w.map('comments').get('c-1'), pin('c-1', 'sam', 'Sam Rivera', [msg('m-1', 'sam', 'Sam Rivera', 'hello')]))
    await until(() => sam.doc.getMap('comments').get('c-1')?.authorName === 'Sam Rivera', 2000, "the name on Sam's tab")
    await w.close()
})

test("a note's maker, a vote and a facilitation round are the writer's own", async () => {
    const w = await world()
    const ann = await w.open('{"sub":"ann"}')
    const sam = await w.open('{"sub":"sam"}')
    set(ann, 'noteAuthors', 'n-ann', { id: 'ann', name: 'Ann Lee' })
    set(ann, 'facilitation', 'voting', { id: 'v-1', perPerson: 3, byId: 'ann', byName: 'Ann Lee' })
    await until(() => sam.doc.getMap('facilitation').has('voting'), 2000, "Ann's round on Sam's tab")
    set(sam, 'noteAuthors', 'n-1', { id: 'ann', name: 'Ann Lee' })
    set(sam, 'noteAuthors', 'n-ann', { id: 'sam', name: 'Sam Rivera' })
    set(sam, 'votes', 'v-1:ann', ['e-1', 'e-2'])
    set(sam, 'facilitation', 'private', { id: 'p-1', byId: 'ann', byName: 'Ann Lee', startedAt: 1 })
    set(sam, 'facilitation', 'timer', { endsAt: 9, durationMs: 60000, byName: 'Ann Lee' })
    await settle(w, sam)
    assert.equal(w.map('noteAuthors').has('n-1'), false, 'a note made as Ann')
    assert.deepEqual(w.map('noteAuthors').get('n-ann'), { id: 'ann', name: 'Ann Lee' }, "Ann's note taken over")
    assert.equal(w.map('votes').has('v-1:ann'), false, 'a vote as Ann')
    assert.equal(w.map('facilitation').has('private'), false, 'a private round as Ann')
    assert.equal(w.map('facilitation').get('timer')?.byName, 'Sam Rivera', 'a timer started is his, by name')
    await until(() => w.lines().length === 4, 2000, 'four log lines')
    assert.ok(['noteAuthors', 'noteAuthors', 'votes', 'facilitation'].every((m) => w.lines().some((l) => l.includes(m))), w.lines().join('\n'))
    // His own stand, and so does closing Ann's round (its maker kept).
    set(sam, 'noteAuthors', 'n-2', { id: 'sam', name: 'Sam Rivera' })
    set(sam, 'votes', 'v-1:sam', ['e-1'])
    set(sam, 'facilitation', 'voting', { id: 'v-1', perPerson: 3, byId: 'ann', byName: 'Ann Lee', closedAt: 5 })
    await settle(w, sam)
    assert.deepEqual(w.map('noteAuthors').get('n-2'), { id: 'sam', name: 'Sam Rivera' })
    assert.deepEqual(w.map('votes').get('v-1:sam'), ['e-1'])
    assert.equal(w.map('facilitation').get('voting')?.closedAt, 5)
    assert.equal(w.lines().length, 4, 'nothing more logged')
    await w.close()
})

test("a call's guest who may draw writes as themselves too", async () => {
    const w = await world()
    const guest = await w.open(GUEST)
    set(guest, 'noteAuthors', 'n-g', { id: 'guest-g-1', name: 'Guest: Gil' })
    set(guest, 'noteAuthors', 'n-s', { id: 'sam', name: 'Sam Rivera' })
    set(guest, 'comments', 'c-s', pin('c-s', 'sam', 'Sam Rivera', [msg('m-1', 'sam', 'Sam Rivera', 'ship it')]))
    await settle(w, guest)
    assert.deepEqual(w.map('noteAuthors').get('n-g'), { id: 'guest-g-1', name: 'Guest: Gil' }, "the guest's own note")
    assert.equal(w.map('noteAuthors').has('n-s'), false, 'a note made as Sam')
    assert.equal(w.map('comments').has('c-s'), false, 'a comment as Sam')
    await until(() => w.lines().length === 2, 2000, 'two log lines')
    await w.close()
})

test("a doc's record of where an agent wrote is this service's alone", async () => {
    const w = await world()
    const ann = await w.open('{"sub":"ann"}', DOC)
    set(ann, 'agentEdits', 'e-forged', { agent: 'agent-1', asker: 'sam', run: 'r-1', at: 1, from: '', to: '' })
    await settle(w, ann, DOC)
    assert.equal(w.map('agentEdits', DOC).has('e-forged'), false, 'a record written over the socket is put back')
    await until(() => w.lines().some((l) => l.includes(DOC) && l.includes('agentEdits')), 2000, 'the log line')
    await w.close()
})

test("a reactions value of any other shape than emoji -> [user ids] is put back, on anyone's message", async () => {
    const w = await world()
    const ann = await w.open('{"sub":"ann"}')
    const sam = await w.open('{"sub":"sam"}')
    const anns = pin('c-a', 'ann', 'Ann Lee', [msg('m-a', 'ann', 'Ann Lee', 'hers')])
    const sams = pin('c-s', 'sam', 'Sam Rivera', [msg('m-s', 'sam', 'Sam Rivera', 'his')])
    set(ann, 'comments', 'c-a', anns)
    set(sam, 'comments', 'c-s', sams)
    await until(() => sam.doc.getMap('comments').has('c-a'), 2000, "Ann's pin on Sam's tab")
    const shapes = { 'a string': { '👍': 'xxxxxxxxxx' }, 'an array': [['ann', 'bo']], 'an object': { '👍': { length: 1 } } }
    for (const [what, reactions] of Object.entries(shapes)) {
        for (const p of [anns, sams]) {
            set(sam, 'comments', p.id, { ...p, messages: [{ ...p.messages[0], reactions }] })
            await settle(w, sam)
            assert.deepEqual(w.map('comments').get(p.id), p, `${what} on ${p.authorName}'s message is put back`)
        }
    }
    assert.equal(w.lines().length, 6, 'each one logged')
    await w.close()
})

// Private mode (miro-25): a held note carries its author as customData.privateBy
// and privateName, and every tab shows "Private · <name>" on it until they open it.
const held = (id, by, name, version = 1, extra = {}) => ({ id, type: 'rectangle', x: 0, y: 0, version, versionNonce: version, isDeleted: false, customData: { kind: 'note', ...(by === undefined ? {} : { privateBy: by, privateName: name }) }, ...extra })
const heldBy = (w, id) => w.map('elements').get(id)?.customData?.privateBy

test("a note held for someone else is put back and logged; one's own held note, its reveal and others' edits stand", async () => {
    const w = await world()
    const ann = await w.open('{"sub":"ann"}')
    const sam = await w.open('{"sub":"sam"}')
    const guest = await w.open(GUEST)
    // (a) a member and a call guest each write a note held for Ann
    set(sam, 'elements', 'n-s', held('n-s', 'ann', 'Ann Lee'))
    set(guest, 'elements', 'n-g', held('n-g', 'ann', 'Ann Lee'))
    await settle(w, sam)
    await settle(w, guest)
    for (const id of ['n-s', 'n-g']) assert.ok(w.map('elements').get(id) === undefined || w.map('elements').get(id).isDeleted === true, `${id} held for Ann is put back`)
    await until(() => w.lines().filter((l) => l.includes('elements')).length === 2, 2000, 'two log lines')
    // (b) Ann's own held note stands, with her API name
    set(ann, 'elements', 'n-a', held('n-a', 'ann', 'Ann (tab)'))
    await until(() => w.map('elements').get('n-a')?.customData?.privateName === 'Ann Lee', 2000, "Ann's own held note, her name stamped")
    await until(() => sam.doc.getMap('elements').get('n-a')?.customData?.privateName === 'Ann Lee', 2000, "the stamp on Sam's tab")
    const v = () => w.map('elements').get('n-a').version
    // (d) Sam's edit that leaves the hold as it is stands
    set(sam, 'elements', 'n-a', { ...w.map('elements').get('n-a'), x: 50, version: v() + 1 })
    await settle(w, sam)
    assert.equal(w.map('elements').get('n-a').x, 50, "Sam's move stands")
    assert.equal(heldBy(w, 'n-a'), 'ann')
    // (e) Sam taking the hold off Ann's note is put back
    const ver = v()
    set(sam, 'elements', 'n-a', { ...held('n-a', undefined, undefined, ver + 1), x: 50 })
    await settle(w, sam)
    assert.equal(heldBy(w, 'n-a'), 'ann', "Ann's hold put back")
    assert.ok(v() > ver + 1, 'as a newer version every tab takes')
    assert.equal(w.lines().filter((l) => l.includes('elements')).length, 3)
    // (c) Ann's own reveal takes the hold off, and stands as she wrote it
    set(ann, 'elements', 'n-a', { ...held('n-a', undefined, undefined, v() + 1), x: 50 })
    await settle(w, ann)
    assert.equal(heldBy(w, 'n-a'), undefined, "Ann's reveal stands")
    // (d) and Sam deleting a note held by Ann stands
    set(ann, 'elements', 'n-b', held('n-b', 'ann', 'Ann Lee'))
    await until(() => sam.doc.getMap('elements').has('n-b'), 2000, "Ann's second note on Sam's tab")
    set(sam, 'elements', 'n-b', { ...held('n-b', 'ann', 'Ann Lee', 2), isDeleted: true })
    await settle(w, sam)
    assert.equal(w.map('elements').get('n-b').isDeleted, true, "Sam's delete stands")
    assert.equal(w.lines().filter((l) => l.includes('elements')).length, 3, 'nothing more logged')
    await w.close()
})

test("renaming a note still held for someone else is put back as a newer version of theirs, and logged", async () => {
    const w = await world()
    const ann = await w.open('{"sub":"ann"}')
    const sam = await w.open('{"sub":"sam"}')
    set(ann, 'elements', 'n-a', held('n-a', 'ann', 'Ann Lee'))
    await until(() => sam.doc.getMap('elements').has('n-a'), 2000, "Ann's held note on Sam's tab")
    const was = w.map('elements').get('n-a')
    set(sam, 'elements', 'n-a', { ...was, version: was.version + 1, customData: { ...was.customData, privateName: 'Bob' } })
    await settle(w, sam)
    const now = w.map('elements').get('n-a')
    assert.equal(now.customData.privateName, 'Ann Lee', "Ann's name on her held note put back")
    assert.equal(now.customData.privateBy, 'ann')
    assert.ok(now.version > was.version + 1, 'as a newer version every tab takes')
    await until(() => w.lines().some((l) => l.includes('elements.customData.privateName')), 2000, 'the log line')
    assert.ok(!w.lines().some((l) => l.includes('Bob')), 'no text in the log')
    await w.close()
})
