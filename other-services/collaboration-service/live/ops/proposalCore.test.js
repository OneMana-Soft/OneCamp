import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { readFileSync } from 'node:fs'
import * as Y from 'yjs'
import { getSchema } from '@tiptap/core'
import { Slice, Fragment } from '@tiptap/pm/model'
import { Transform } from '@tiptap/pm/transform'
import { initProseMirrorDoc, prosemirrorJSONToYXmlFragment, updateYFragment } from 'y-prosemirror'
import { COLLAB_EXTENSIONS } from '../../extensions.js'
import { EVERY_BLOCK } from '../../everyBlock.fixture.js'
import {
    applyPlaced, childrenOf, diffBlocks, diffRuns, encodePlaces, encodeTarget, insertBelow, LIMITS, locate,
    nodeKey, nodesFromJSON, nodesKey, proposalFromReplacement, resolveRelative, siblingsBetween,
    targetForRange, validHunks,
} from './proposalCore.js'

// The core is the web app's lib/proposals/core.js byte for byte. Change both,
// then this pin and the web's (lib/proposals/core.test.ts).
export const CORE_SHA256 = 'cf97ede94c524299ac16d6e46813fef05a21a943fa06e5d5b3202c0e39f47f77'

const schema = getSchema(COLLAB_EXTENSIONS)
const t = (text, marks) => (marks ? { type: 'text', text, marks } : { type: 'text', text })
const p = (...texts) => ({ type: 'paragraph', content: texts.map((x) => (typeof x === 'string' ? t(x) : x)) })
const h = (level, text) => ({ type: 'heading', attrs: { level }, content: [t(text)] })
const docOf = (...blocks) => schema.nodeFromJSON({ type: 'doc', content: blocks })
const blocks = (...b) => b.map((x) => schema.nodeFromJSON(x))

// applyAll applies the chosen hunks (all when none are named) to a doc whose
// target starts at `start` and is still the base.
function applyAt(doc, start, base, hunks, ids) {
    const r = locate(doc, { from: start, to: start + base.reduce((n, b) => n + b.nodeSize, 0) }, base.map((b) => b.toJSON()), hunks, ids, null)
    const tr = new Transform(doc)
    applyPlaced(tr, r.placed)
    return { doc: tr.doc, ...r }
}

test('the pinned core is the one this file runs', () => {
    const sha = createHash('sha256').update(readFileSync(new URL('./proposalCore.js', import.meta.url))).digest('hex')
    assert.equal(sha, CORE_SHA256, 'proposalCore.js changed: copy it to the web app (lib/proposals/core.js) and update both pins')
})

test('runs cover both sides in order', () => {
    const runs = diffRuns(['a', 'b', 'c', 'd'], ['a', 'x', 'c', 'd', 'e'])
    assert.deepEqual(runs.map((r) => [r.same, r.a0, r.a1, r.b0, r.b1]), [
        [true, 0, 1, 0, 1], [false, 1, 2, 1, 2], [true, 2, 4, 2, 4], [false, 4, 4, 4, 5],
    ])
    assert.deepEqual(diffRuns([], []), [])
})

test('a rewritten sentence is word hunks inside the same paragraph', () => {
    const base = blocks(p('The quick brown fox jumps over the dog.'))
    const proposed = blocks(p('The slow brown fox leaps over the lazy dog.'))
    const hunks = diffBlocks(base, proposed)
    assert.deepEqual(hunks.map((x) => [x.level, x.op]), [['inline', 'replace'], ['inline', 'replace'], ['inline', 'insert']])
    assert.ok(validHunks(base, hunks))
    const doc = docOf(h(1, 'Title'), ...base.map((b) => b.toJSON()), p('After.'))
    const start = doc.child(0).nodeSize
    const all = applyAt(doc, start, base, hunks)
    assert.equal(all.doc.child(1).textContent, 'The slow brown fox leaps over the lazy dog.')
    assert.equal(nodesKey([all.doc.child(1)]), nodesKey(proposed))
    // Two of three: only those.
    const some = applyAt(doc, start, base, hunks, ['h1', 'h3'])
    assert.equal(some.doc.child(1).textContent, 'The slow brown fox jumps over the lazy dog.')
    assert.equal(some.doc.child(0).textContent, 'Title')
    assert.equal(some.doc.child(2).textContent, 'After.')
})

test('blocks are inserted, removed and swapped, and accepting all gives the proposal', () => {
    const base = blocks(h(2, 'Plan'), p('Ship Thursday.'), p('Tell sales.'), p('Old note.'))
    const proposed = blocks(h(3, 'Plan'), p('Ship Friday.'), p('Tell sales.'), p('Write the notes.'), p('Celebrate.'))
    const hunks = diffBlocks(base, proposed)
    assert.ok(validHunks(base, hunks))
    const doc = docOf(...base.map((b) => b.toJSON()))
    const all = applyAt(doc, 0, base, hunks)
    assert.equal(nodesKey(childrenOf(all.doc)), nodesKey(proposed))
})

test('lists and marks: the inside of a container is compared, marks count as content', () => {
    const li = (...x) => ({ type: 'listItem', content: [p(...x)] })
    const base = blocks({ type: 'bulletList', content: [li('One'), li('Two'), li('Three')] })
    const proposed = blocks({ type: 'bulletList', content: [li('One'), li('Two ', t('now', [{ type: 'bold' }])), li('Three'), li('Four')] })
    const hunks = diffBlocks(base, proposed)
    assert.deepEqual(hunks.map((x) => [x.level, x.op, x.path]), [['inline', 'insert', [0, 1, 0]], ['block', 'insert', [0]]])
    const all = applyAt(docOf(...base.map((b) => b.toJSON())), 0, base, hunks)
    assert.equal(nodesKey(childrenOf(all.doc)), nodesKey(proposed))
})

test('every block the editor writes survives a diff and an apply', () => {
    const base = nodesFromJSON(schema, EVERY_BLOCK.content)
    const proposed = base.map((b, i) => (i === 1 ? schema.nodeFromJSON(p('Bold and plain now.')) : b))
    proposed.push(schema.nodeFromJSON(p('A new last line.')))
    const hunks = diffBlocks(base, proposed)
    assert.ok(validHunks(base, hunks))
    const all = applyAt(schema.nodeFromJSON(EVERY_BLOCK), 0, base, hunks)
    assert.equal(nodesKey(childrenOf(all.doc)), nodesKey(proposed))
})

test('a block id is identity, not content', () => {
    const a = schema.nodeFromJSON(p('Same.'))
    assert.equal(nodeKey(a), nodeKey(schema.nodeFromJSON(p('Same.'))))
})

test('hunks that do not match their base are refused', () => {
    const base = blocks(p('Ship Thursday.'))
    const hunks = diffBlocks(base, blocks(p('Ship Friday.')))
    assert.ok(validHunks(base, hunks))
    const forged = hunks.map((x) => ({ ...x, before: [t('Monday')] }))
    assert.equal(validHunks(base, forged), false)
    const overlapping = [hunks[0], { ...hunks[0], id: 'h9' }]
    assert.equal(validHunks(base, overlapping), false)
})

test('a replacement of a selection proposes whole blocks', () => {
    const doc = docOf(p('Keep this.'), p('We ship on Thursday, maybe.'), p('Keep that.'))
    const from = 1 + doc.child(0).nodeSize + 'We ship on '.length
    const to = from + 'Thursday, maybe'.length
    const { target, base, proposed } = proposalFromReplacement(new Transform(doc), from, to, new Slice(Fragment.from(schema.text('Friday')), 0, 0))
    assert.deepEqual(target, targetForRange(doc, from, to))
    assert.equal(base.length, 1)
    assert.equal(proposed[0].textContent, 'We ship on Friday.')
    const hunks = diffBlocks(base, proposed)
    assert.equal(hunks.length, 1)
    assert.equal(hunks[0].level, 'inline')
})

// The live doc in Yjs, with the binding's mapping, as the service and the web
// app's editor both hold it.
function live(json) {
    const ydoc = new Y.Doc()
    const fragment = ydoc.getXmlFragment('default')
    prosemirrorJSONToYXmlFragment(schema, json, fragment)
    return { ydoc, fragment }
}
const view = ({ fragment }) => initProseMirrorDoc(fragment, schema)

function propose(state, from, to, proposedBlocks) {
    const { doc, meta } = view(state)
    const target = { from, to }
    const base = siblingsBetween(doc, from, to)
    const hunks = encodePlaces(state.fragment, doc, from, base, diffBlocks(base, proposedBlocks))
    return { anchor: encodeTarget(state.fragment, doc, target), base: base.map((b) => b.toJSON()), hunks, meta }
}

// applyLive is the service's apply: find the target and the hunks in the live
// doc, apply in one transaction, write back with y-prosemirror.
function applyLive(state, prop, ids) {
    const { doc, meta } = view(state)
    const resolve = (e) => resolveRelative(state.ydoc, state.fragment, meta.mapping, e)
    const from = resolve(prop.anchor.start)
    const to = resolve(prop.anchor.end)
    const r = locate(doc, from !== null && to !== null ? { from, to } : null, prop.base, prop.hunks, ids, resolve)
    if (r.conflicts.length) return r
    const tr = new Transform(doc)
    applyPlaced(tr, r.placed)
    state.ydoc.transact(() => updateYFragment(state.ydoc, state.fragment, tr.doc, meta))
    return r
}

const textOf = (state) => view(state).doc.textBetween(0, view(state).doc.content.size, '|')

test('yjs: an edit above the target moves it, and the hunks still land', () => {
    const state = live({ type: 'doc', content: [p('Intro.'), p('We ship on Thursday.'), p('End.')] })
    const { doc } = view(state)
    const start = doc.child(0).nodeSize
    const prop = propose(state, start, start + doc.child(1).nodeSize, blocks(p('We ship on Friday.')))
    // Someone types above and inserts a whole block.
    const first = state.fragment.get(0).get(0)
    first.insert(0, 'A longer ')
    const extra = new Y.XmlElement('paragraph')
    extra.insert(0, [new Y.XmlText('Inserted above.')])
    state.fragment.insert(1, [extra])
    const r = applyLive(state, prop)
    assert.deepEqual(r.conflicts, [])
    assert.equal(textOf(state), 'A longer Intro.|Inserted above.|We ship on Friday.|End.')
})

test('yjs: an edit inside the target but away from a hunk keeps it; one inside the hunk conflicts', () => {
    const state = live({ type: 'doc', content: [p('We ship on Thursday and tell sales.')] })
    const { doc } = view(state)
    const prop = propose(state, 0, doc.child(0).nodeSize, blocks(p('We ship on Friday and tell everyone.')))
    assert.equal(prop.hunks.length, 2)
    const text = state.fragment.get(0).get(0)
    text.insert(0, 'Note: ') // away from both hunks
    const r = applyLive(state, prop, ['h1'])
    assert.equal(r.exact, false)
    assert.deepEqual(r.conflicts, [])
    assert.equal(textOf(state), 'Note: We ship on Friday and tell sales.')
    // Now someone edits the second hunk's own word.
    const where = text.toString().indexOf('sales')
    text.delete(where, 5)
    text.insert(where, 'marketing')
    const r2 = applyLive(state, prop, ['h2'])
    assert.deepEqual(r2.conflicts, ['h2'])
    assert.equal(textOf(state), 'Note: We ship on Friday and tell marketing.')
})

test('yjs: a deleted target is a conflict, and Insert below puts the proposal where it was', () => {
    const state = live({ type: 'doc', content: [p('Before.'), p('Target text.'), p('After.')] })
    const { doc } = view(state)
    const start = doc.child(0).nodeSize
    const prop = propose(state, start, start + doc.child(1).nodeSize, blocks(p('Better text.')))
    state.fragment.delete(1, 1)
    const r = applyLive(state, prop)
    assert.deepEqual(r.conflicts, ['h1'])
    const { doc: now, meta } = view(state)
    const at = resolveRelative(state.ydoc, state.fragment, meta.mapping, prop.anchor.end)
    const tr = new Transform(now)
    insertBelow(tr, at ?? now.content.size, [p('Better text.')])
    assert.equal(tr.doc.textBetween(0, tr.doc.content.size, '|'), 'Before.|Better text.|After.')
})

test('yjs: a server apply equals a client apply', () => {
    const json = { type: 'doc', content: [h(2, 'Launch'), p('We ship on Thursday.'), p('Tell sales.')] }
    const state = live(json)
    const { doc } = view(state)
    const proposed = blocks(h(2, 'Launch'), p('We ship on Friday.'), p('Tell sales and support.'), p('Celebrate.'))
    const prop = propose(state, 0, doc.content.size, proposed)
    // The client: the same doc, applied as a ProseMirror transaction.
    const client = new Transform(schema.nodeFromJSON(json))
    applyPlaced(client, locate(client.doc, { from: 0, to: client.doc.content.size }, prop.base, prop.hunks, null, null).placed)
    applyLive(state, prop)
    assert.equal(nodesKey(childrenOf(view(state).doc)), nodesKey(childrenOf(client.doc)))
    assert.equal(nodesKey(childrenOf(client.doc)), nodesKey(proposed))
})

// workOf runs f and counts its diff work, in the steps diffBlocks' budget
// counts: the cells of every table diffRuns makes, and every character cut
// into words (wordsOf's pattern), at LIMITS.cellsPerCharacter cells each. A
// count, not a clock, so it is the same on any machine at any load.
function workOf(f) {
    const Real = globalThis.Uint32Array
    const match = String.prototype.match
    let cells = 0
    let chars = 0
    globalThis.Uint32Array = class extends Real {
        constructor(n, ...rest) {
            super(n, ...rest)
            if (typeof n === 'number') cells += n
        }
    }
    String.prototype.match = function (re) {
        if (re instanceof RegExp && re.source.startsWith('[\\p{L}')) chars += this.length
        return match.call(this, re)
    }
    try {
        const result = f()
        return { result, cells, chars, work: cells + chars * (LIMITS.cellsPerCharacter ?? 16) }
    } finally {
        globalThis.Uint32Array = Real
        String.prototype.match = match
    }
}

// words is a paragraph of n one-letter words: as many words as a paragraph of
// its length can hold, so a pair of them is the largest table per byte.
const words = (n, w) => p(Array(n).fill(w).join(' '))

// diffWork is the most one diffBlocks call may spend. Each table has a row and
// a column over its cells, so the count is allowed a little past it.
const diffWork = () => LIMITS.diffWork ?? 2 * LIMITS.diffCells
const withinWork = (work) => work <= diffWork() * 1.01

// Every hunk of one diffBlocks call fits its base, and applying them all gives
// the proposal.
function appliesWhole(base, proposed, hunks) {
    assert.ok(validHunks(base, hunks), 'the hunks fit their base')
    const all = applyAt(docOf(...base.map((b) => b.toJSON())), 0, base, hunks)
    assert.deepEqual(all.conflicts, [])
    assert.equal(nodesKey(childrenOf(all.doc)), nodesKey(proposed))
}

// The largest suggestion an agent may send (20,000 characters, business/AI
// docEdit.go docEditMaxContentRunes) against a long section, every pair of
// paragraphs a table just under LIMITS.diffCells: 273M cells, 12.6 s at a load
// of 84, during which the service answered no editor (16 Oct 2026,
// logs/review-docs-be/proposal-diff-bench.log).
test('an agent\'s largest suggestion against a long section is diffed within one budget', () => {
    const base = Array.from({ length: 70 }, () => schema.nodeFromJSON(words(7000, 'a')))
    const proposed = Array.from({ length: 70 }, () => schema.nodeFromJSON(words(140, 'b')))
    const { result: hunks, work, cells, chars } = workOf(() => diffBlocks(base, proposed))
    assert.ok(withinWork(work), `diffed in ${work} steps (${cells} cells, ${chars} characters cut), past one call's ${diffWork()}`)
    appliesWhole(base, proposed, hunks)
})

// Two hundred pairs of paragraphs, each a full table: 800M cells, 26 s.
test('two hundred paragraphs rewritten whole are diffed within one budget', () => {
    const base = Array.from({ length: 200 }, () => schema.nodeFromJSON(words(1000, 'a')))
    const proposed = Array.from({ length: 200 }, () => schema.nodeFromJSON(words(1000, 'b')))
    const { result: hunks, work, cells, chars } = workOf(() => diffBlocks(base, proposed))
    assert.ok(withinWork(work), `diffed in ${work} steps (${cells} cells, ${chars} characters cut), past one call's ${diffWork()}`)
    appliesWhole(base, proposed, hunks)
})

// A diff of more than LIMITS.hunks hunks becomes one hunk for the whole target,
// so once it has more, the pairs after are not compared: the answer is the
// same, one hunk.
test('a diff stops comparing once it has more hunks than it keeps', () => {
    // Two changes a paragraph, parted by words that stay: two hunks each.
    const para = (x, y) => {
        const w = Array.from({ length: 100 }, (_, i) => `w${i}`)
        w[25] = x
        w[75] = y
        return p(w.join(' '))
    }
    const pairs = 400
    const n = (i) => String(i).padStart(3, '0') // every pair as long, so each costs the same
    const base = Array.from({ length: pairs }, (_, i) => schema.nodeFromJSON(para(`old${n(i)}`, `was${n(i)}`)))
    const proposed = Array.from({ length: pairs }, (_, i) => schema.nodeFromJSON(para(`new${n(i)}`, `now${n(i)}`)))
    const blockTable = workOf(() => diffRuns(base.map(nodeKey), proposed.map(nodeKey))).work
    const onePair = workOf(() => diffBlocks([base[0]], [proposed[0]])).work - workOf(() => diffRuns([nodeKey(base[0])], [nodeKey(proposed[0])])).work
    assert.equal(diffBlocks([base[0]], [proposed[0]]).length, 2)
    const { result: hunks, work } = workOf(() => diffBlocks(base, proposed))
    // Past LIMITS.hunks after the pair that brings it there: half as many pairs, and one.
    const most = blockTable + (Math.floor(LIMITS.hunks / 2) + 1) * onePair
    assert.ok(work <= most, `compared in ${work} steps, past the ${most} that make ${LIMITS.hunks} hunks`)
    assert.equal(hunks.length, 1)
    assert.equal(hunks[0].level, 'block')
    appliesWhole(base, proposed, hunks)
})
