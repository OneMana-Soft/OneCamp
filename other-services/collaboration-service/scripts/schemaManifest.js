// The schema manifest generator (docs programme I4, A1): takes the manifest
// the web app's block registry generated, checks that this server's schema
// (schema/index.js) is exactly it, and adopts it.
//
//   node scripts/schemaManifest.js --from <web>/components/minimal-tiptap/schema-manifest.json
//   node scripts/schemaManifest.js --check
//
// Adopting writes schema-manifest.json byte for byte as the web's (the two
// copies are compared byte-equal) and appends its hash to schema-history.json,
// so a tab built before the change sends a hash this server knows as older
// and opens docs read-only until it reloads (schemaGate.js). --check writes
// nothing: the stored manifest must be this server's schema and the history
// must end with its hash.
//
// The web side regenerates its copy with `pnpm schema:manifest`.

import { readFileSync, writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import path from 'node:path'
import { getSchema } from '@tiptap/core'
import { COLLAB_EXTENSIONS } from '../schema/index.js'
import { manifestOf } from '../schemaManifest.js'
import { manifestHash } from '../schemaGate.js'

const SERVICE = path.join(path.dirname(fileURLToPath(import.meta.url)), '..')

// serverManifest is this server's schema as a manifest.
export const serverManifest = (extensions = COLLAB_EXTENSIONS) => manifestOf(getSchema(extensions))

// manifestDiff lists how two manifests differ, one line each; none when equal.
export function manifestDiff(want, have) {
    const out = []
    for (const kind of ['nodes', 'marks']) {
        const a = want[kind] || {}
        const b = have[kind] || {}
        for (const name of Object.keys(a)) {
            if (!b[name]) out.push(`the server has no ${kind.slice(0, -1)} "${name}"`)
            else if (JSON.stringify(a[name]) !== JSON.stringify(b[name])) out.push(`${kind.slice(0, -1)} "${name}": the web has ${JSON.stringify(a[name])}, the server ${JSON.stringify(b[name])}`)
        }
        for (const name of Object.keys(b)) if (!a[name]) out.push(`the web has no ${kind.slice(0, -1)} "${name}"`)
    }
    return out
}

// adopt checks the web's manifest against this server's schema and, when they
// agree, writes it and records its hash. It returns what it did.
export function adopt({ fromText, dir = SERVICE, extensions = COLLAB_EXTENSIONS }) {
    const want = JSON.parse(fromText)
    const differences = manifestDiff(want, serverManifest(extensions))
    if (differences.length) {
        const err = new Error(`the server's schema is not the web's:\n  ${differences.join('\n  ')}\nAdd or change the block's file in schema/ (and its line in schema/index.js), then run this again.`)
        err.differences = differences
        throw err
    }
    const manifestPath = path.join(dir, 'schema-manifest.json')
    const historyPath = path.join(dir, 'schema-history.json')
    const hash = manifestHash(fromText)
    const history = JSON.parse(readFileSync(historyPath, 'utf8'))
    const hashes = Array.isArray(history.hashes) ? history.hashes : []
    let recorded = false
    if (hashes.at(-1) !== hash) {
        if (hashes.includes(hash)) throw new Error(`this manifest (${hash}) was shipped before, and is not the current one: going back to an older schema needs a decision, not this script`)
        hashes.push(hash)
        writeFileSync(historyPath, JSON.stringify({ ...history, hashes }, null, 2) + '\n')
        recorded = true
    }
    writeFileSync(manifestPath, fromText)
    return { hash, recorded }
}

// check is --check: the stored manifest is this server's schema, and the
// history ends with its hash.
export function check({ dir = SERVICE, extensions = COLLAB_EXTENSIONS } = {}) {
    const text = readFileSync(path.join(dir, 'schema-manifest.json'), 'utf8')
    const problems = manifestDiff(JSON.parse(text), serverManifest(extensions))
    const hashes = JSON.parse(readFileSync(path.join(dir, 'schema-history.json'), 'utf8')).hashes || []
    if (hashes.at(-1) !== manifestHash(text)) problems.push('schema-history.json does not end with the manifest\'s hash: adopt it with --from')
    return problems
}

if (process.argv[1] && fileURLToPath(import.meta.url) === path.resolve(process.argv[1])) {
    const args = process.argv.slice(2)
    const from = args.includes('--from') ? args[args.indexOf('--from') + 1] : null
    try {
        if (from) {
            const { hash, recorded } = adopt({ fromText: readFileSync(from, 'utf8') })
            console.log(`schema-manifest.json is the web's; ${recorded ? 'its hash is recorded' : 'its hash was already the current one'}: ${hash}`)
        } else if (args.includes('--check')) {
            const problems = check()
            if (problems.length) {
                console.error(problems.join('\n'))
                process.exit(1)
            }
            console.log('schema-manifest.json is this server\'s schema, and the history ends with it')
        } else {
            console.error('usage: node scripts/schemaManifest.js --from <web schema-manifest.json> | --check')
            process.exit(2)
        }
    } catch (e) {
        console.error(e.message)
        process.exit(1)
    }
}
