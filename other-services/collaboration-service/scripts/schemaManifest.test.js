import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, readFileSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { manifestHash } from '../schemaGate.js'
import { adopt, check, manifestDiff, serverManifest } from './schemaManifest.js'

// The manifest generator: the web's block registry generates the manifest,
// and this server adopts it only when its own schema is exactly that.
const SERVICE = path.join(path.dirname(fileURLToPath(import.meta.url)), '..')
const CURRENT = readFileSync(path.join(SERVICE, 'schema-manifest.json'), 'utf8')

function scratch(history) {
    const dir = mkdtempSync(path.join(tmpdir(), 'schema-manifest-'))
    writeFileSync(path.join(dir, 'schema-history.json'), JSON.stringify({ hashes: history }, null, 2) + '\n')
    writeFileSync(path.join(dir, 'schema-manifest.json'), '{}\n')
    return dir
}

test("this service's schema is its stored manifest, and the history ends with it", () => {
    assert.deepEqual(check(), [])
    assert.deepEqual(manifestDiff(JSON.parse(CURRENT), serverManifest()), [])
})

test('adopting writes the web file byte for byte and records its hash once', () => {
    const dir = scratch(['an-older-manifest'])
    const first = adopt({ fromText: CURRENT, dir })
    assert.deepEqual(first, { hash: manifestHash(CURRENT), recorded: true })
    assert.equal(readFileSync(path.join(dir, 'schema-manifest.json'), 'utf8'), CURRENT)
    assert.deepEqual(JSON.parse(readFileSync(path.join(dir, 'schema-history.json'), 'utf8')).hashes, ['an-older-manifest', manifestHash(CURRENT)])
    // Again: nothing more to record.
    assert.equal(adopt({ fromText: CURRENT, dir }).recorded, false)
    assert.equal(JSON.parse(readFileSync(path.join(dir, 'schema-history.json'), 'utf8')).hashes.length, 2)
})

test('a manifest the server cannot hold is refused, and says what differs', () => {
    const web = JSON.parse(CURRENT)
    web.nodes.futureBlock = { attrs: ['blockId'], group: 'block', content: null, inline: false, atom: true }
    web.nodes.image.attrs = web.nodes.image.attrs.filter((a) => a !== 'caption')
    const dir = scratch([])
    assert.throws(() => adopt({ fromText: JSON.stringify(web, null, 2) + '\n', dir }), (e) => {
        assert.match(e.message, /the server has no node "futureBlock"/)
        assert.match(e.message, /node "image"/)
        return true
    })
    assert.equal(readFileSync(path.join(dir, 'schema-manifest.json'), 'utf8'), '{}\n', 'nothing written')
})

test('going back to a manifest shipped before is refused', () => {
    const dir = scratch([manifestHash(CURRENT), 'a-newer-manifest'])
    assert.throws(() => adopt({ fromText: CURRENT, dir }), /shipped before/)
})
