import { test } from 'node:test'
import assert from 'node:assert/strict'
import { existsSync, readFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { getSchema } from '@tiptap/core'
import { COLLAB_EXTENSIONS } from './extensions.js'
import { manifestOf } from './schemaManifest.js'
import { EVERY_BLOCK, typesIn } from './everyBlock.fixture.js'

// The server must know every node and mark the web editor writes, with the
// same attributes, or saving a doc that holds one fails (11 Oct 2026: `/table`,
// `+doc`, `#channel`, polls, charts, diffs and recordings, and image captions
// were dropped). schema-manifest.json is the web editor's schema; the web repo
// keeps a byte-equal copy (components/minimal-tiptap/schema-manifest.json) and
// fails its own test when its editor drifts from it. A block added on either
// side alone fails one of the two.
const here = path.dirname(fileURLToPath(import.meta.url))
const MANIFEST = path.join(here, 'schema-manifest.json')
const manifest = JSON.parse(readFileSync(MANIFEST, 'utf8'))
const actual = manifestOf(getSchema(COLLAB_EXTENSIONS))

test('the server has exactly the node types of the web editor', () => {
    assert.deepEqual(Object.keys(actual.nodes), Object.keys(manifest.nodes))
})

test('the server has exactly the mark types of the web editor', () => {
    assert.deepEqual(Object.keys(actual.marks), Object.keys(manifest.marks))
})

test('every node and mark has the same attributes and placement', () => {
    for (const [name, want] of Object.entries(manifest.nodes)) assert.deepEqual(actual.nodes[name], want, `node ${name}`)
    for (const [name, want] of Object.entries(manifest.marks)) assert.deepEqual(actual.marks[name], want, `mark ${name}`)
})

test('the round-trip fixture holds every type in the manifest', () => {
    const used = typesIn(EVERY_BLOCK)
    assert.deepEqual(Object.keys(manifest.nodes).filter((n) => !used.nodes.has(n)), [], 'nodes missing from everyBlock.fixture.js')
    assert.deepEqual(Object.keys(manifest.marks).filter((m) => !used.marks.has(m)), [], 'marks missing from everyBlock.fixture.js')
})

// Pointed at a web checkout (ONECAMP_FE_DIR), the two copies must be equal.
const feDir = process.env.ONECAMP_FE_DIR
test('is the same file as the web repo copy', { skip: !feDir && 'set ONECAMP_FE_DIR to compare with the web repo' }, () => {
    const copy = path.join(feDir, 'components/minimal-tiptap/schema-manifest.json')
    assert.ok(existsSync(copy), `${copy} exists`)
    assert.equal(readFileSync(copy, 'utf8'), readFileSync(MANIFEST, 'utf8'))
})

// The schema gate (schemaGate.js) tells a tab built against an older manifest
// to reload; it knows "older" from this list, so every manifest must be in it.
test('schema-history.json ends with the current manifest', async () => {
    const { SCHEMA_HASH, SCHEMA_HISTORY, manifestHash } = await import('./schemaGate.js')
    assert.equal(SCHEMA_HASH, manifestHash(readFileSync(MANIFEST, 'utf8')))
    assert.equal(SCHEMA_HISTORY.at(-1), SCHEMA_HASH, 'append the new manifest hash to schema-history.json')
    assert.equal(new Set(SCHEMA_HISTORY).size, SCHEMA_HISTORY.length)
})
