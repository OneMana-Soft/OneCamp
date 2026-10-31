// Makes yjs_states.json: whole Yjs states (Y.encodeStateAsUpdate, the first
// encoding, as the collaboration service stores a board) and, for each, the
// root maps a Y.Doc that applied it shows, for scene.Maps's tests. A state
// marked refuse is one Maps must not read: an update that isn't a whole state.
//
// Run it with the collaboration service's yjs (13.x), from this folder:
//   YJS=../../../../other-services/collaboration-service/node_modules/yjs/dist/yjs.mjs node yjs_states.mjs > yjs_states.json
const Y = await import(process.env.YJS ?? 'yjs')

const states = []
const b64 = (u8) => Buffer.from(u8).toString('base64')
const add = (name, doc, roots) => {
  const want = {}
  for (const r of roots) want[r] = doc.getMap(r).toJSON()
  states.push({ name, state: b64(Y.encodeStateAsUpdate(doc)), want })
}
const refuse = (name, update) => states.push({ name, state: b64(update), refuse: true })
const docOf = (client, gc = true) => {
  const d = new Y.Doc({ gc })
  d.clientID = client
  return d
}
const syncAll = (...docs) => {
  for (const a of docs) for (const b of docs) if (a !== b) Y.applyUpdate(b, Y.encodeStateAsUpdate(a, Y.encodeStateVector(b)))
}
// A small seeded generator (mulberry32), so the file is the same each run.
const random = (seed) => () => {
  seed = (seed + 0x6d2b79f5) | 0
  let t = Math.imul(seed ^ (seed >>> 15), 1 | seed)
  t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t
  return ((t ^ (t >>> 14)) >>> 0) / 4294967296
}

// Every kind of value an element holds, and keys beside them of other kinds.
{
  const d = docOf(7)
  const els = d.getMap('elements')
  els.set('a', {
    s: 'héllo 👋 — there', t: true, f: false, n: null, small: 3, neg: -70, big: 1791614951123, frac: 1.25,
    tiny: -0.000123, arr: [1, 'x', null, [2, { deep: 'yes' }]], obj: { type: 3, groupIds: [] },
  })
  els.set('b', 'two')
  els.set('c', 42)
  els.set('d', [1, 2, 3])
  add('values', d, ['elements'])
}

// Overwrites and deletions by one screen, with deleted content collected
// (gc, the server's default) and kept (no gc).
for (const gc of [true, false]) {
  const d = docOf(11, gc)
  const els = d.getMap('elements')
  for (let v = 1; v <= 6; v++) els.set('moved', { id: 'moved', version: v, x: v * 10 })
  els.set('gone', { id: 'gone', version: 1 })
  els.delete('gone')
  els.set('back', { id: 'back', version: 1 })
  els.delete('back')
  els.set('back', { id: 'back', version: 3 })
  d.transact(() => {
    for (let i = 0; i < 5; i++) els.set(`batch${i}`, { id: `batch${i}`, version: 1 })
  })
  d.transact(() => {
    for (let i = 0; i < 5; i += 2) els.delete(`batch${i}`)
  })
  add(gc ? 'overwrites, collected' : 'overwrites, kept', d, ['elements'])
}

// Three screens set the same keys at once, and delete what another changes;
// client ids as large as Yjs makes them.
{
  const docs = [docOf(4294967295), docOf(3000000000), docOf(5)]
  docs[0].getMap('elements').set('shared', { by: 'base', version: 1 })
  docs[0].getMap('elements').set('doomed', { by: 'base', version: 1 })
  syncAll(...docs)
  docs.forEach((d, i) => d.getMap('elements').set('shared', { by: `screen ${i}`, version: 2 }))
  docs[1].getMap('elements').delete('doomed')
  docs[2].getMap('elements').set('doomed', { by: 'screen 2', version: 2 })
  docs.forEach((d, i) => d.getMap('elements').set(`own ${i}`, { by: `screen ${i}` }))
  syncAll(...docs)
  add('three screens at once', docs[0], ['elements'])
}

// Shared types of their own beside the maps (a nested map, text with marks
// and an embed, a list, a doc's XML, bytes): read past, never shown as
// values a board keeps.
{
  const d = docOf(21)
  const els = d.getMap('elements')
  els.set('plain', { id: 'plain' })
  const other = d.getMap('other')
  const nested = new Y.Map()
  other.set('nested', nested)
  nested.set('inside', 'a value of a nested map')
  const text = new Y.Text()
  other.set('text', text)
  text.insert(0, 'bold 👋 and plain')
  text.format(0, 4, { bold: true })
  text.insertEmbed(5, { image: 'x.png' })
  other.set('bytes', new Uint8Array([1, 2, 3]))
  d.getArray('list').insert(0, ['one', 2, { three: 3 }])
  const xml = d.getXmlFragment('prosemirror')
  const p = new Y.XmlElement('paragraph')
  xml.insert(0, [p])
  p.insert(0, [new Y.XmlText('words')])
  els.set('after', { id: 'after' })
  add('shared types beside the maps', d, ['elements'])
}

// A board as screens draw it: elements moved, restyled and erased (an
// erased element stays, isDeleted), pictures in files, three screens that
// meet now and then.
{
  const rnd = random(28)
  const docs = [docOf(1001), docOf(2002), docOf(3003)]
  const ids = Array.from({ length: 120 }, (_, i) => `el${i}`)
  const element = (id, version, x, more = {}) => ({
    id, type: 'rectangle', x, y: version * 3, width: 120, height: 80, angle: 0, strokeColor: '#1e1e1e',
    backgroundColor: 'transparent', fillStyle: 'solid', strokeWidth: 2, roughness: 1, opacity: 100,
    groupIds: [], frameId: null, index: `a${id.slice(2).padStart(3, '0')}`, roundness: { type: 3 },
    seed: 12345, version, versionNonce: Math.floor(rnd() * 2 ** 31), isDeleted: false, boundElements: null,
    updated: 1791614951123 + version, link: null, locked: false, ...more,
  })
  ids.forEach((id, i) => docs[i % 3].getMap('elements').set(id, element(id, 1, i * 10)))
  docs[0].getMap('files').set('pic1', { id: 'pic1', objectUuid: '3d6f0c9e-2a49-4d0c-9a54-9b4d3b0f6a11', mimeType: 'image/png' })
  for (let round = 0; round < 400; round++) {
    const d = docs[Math.floor(rnd() * 3)]
    const id = ids[Math.floor(rnd() * ids.length)]
    const cur = d.getMap('elements').get(id)
    const version = (cur?.version ?? 1) + 1
    const r = rnd()
    if (r < 0.1) d.getMap('elements').set(id, element(id, version, cur?.x ?? 0, { isDeleted: true }))
    else d.getMap('elements').set(id, element(id, version, (cur?.x ?? 0) + 5, { strokeColor: r < 0.5 ? '#e03131' : '#1971c2' }))
    if (round % 37 === 0) syncAll(...docs)
  }
  syncAll(...docs)
  add('a board three screens drew', docs[1], ['elements', 'files'])
}

// Not a whole state: what changed since a state vector, and a merge with a
// gap (Yjs writes the gap as a skip).
{
  const d = docOf(31)
  d.getMap('elements').set('first', { v: 1 })
  const sv = Y.encodeStateVector(d)
  d.getMap('elements').set('second', { v: 2 })
  refuse('the changes since a state vector', Y.encodeStateAsUpdate(d, sv))
  const updates = []
  const e = docOf(32)
  e.on('update', (u) => updates.push(u))
  for (let i = 0; i < 3; i++) e.getMap('elements').set(`k${i}`, { i })
  refuse('a merge with a gap', Y.mergeUpdates([updates[0], updates[2]]))
}

process.stdout.write(JSON.stringify(states, null, 1) + '\n')
