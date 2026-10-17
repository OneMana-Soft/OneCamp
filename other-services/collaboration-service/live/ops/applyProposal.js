// The Live Doc Writer's applyProposal op: an edit proposal's pending step, run on
// the live copy of the doc as one Yjs transaction (docs programme, PROGRAM.md §4
// #9 and #19, D-10; A9).
//
// The API decides who may do what, records the step it wants on the proposal
// (its `pending`), and calls this op with the proposal's id. The op reads the
// proposal back from the API, so what it applies is what the API stored, never
// what a caller says it is, and runs the step:
//   - prepare: a proposal made on the server (an agent's suggestion) is turned
//     into hunks against the doc as it is now: the target is found (by block id
//     or heading), its blocks become the base, the agent's HTML the proposed
//     blocks, and the result is posted back to the API. Nothing is written.
//   - apply: the accepted hunks land, each where it is now (proposalCore.js).
//     If any of them no longer can (its text was changed), nothing is written,
//     and the answer is 409 with the hunks that could not.
//   - insert_below: the proposed blocks go below the target, or where it was.
//
// The proposal's own text never leaves the API in a request a browser can make:
// this port is internal (live/server.js), and the API route it reads from takes
// only the internal secret.

import { getSchema } from '@tiptap/core'
import { Transform } from '@tiptap/pm/transform'
import * as Y from 'yjs'
import { initProseMirrorDoc, updateYFragment, yXmlFragmentToProsemirrorJSON } from 'y-prosemirror'
import { createHash } from 'node:crypto'
import { COLLAB_EXTENSIONS } from '../../extensions.js'
import { htmlToDoc, keepersFor, unknownTypes } from '../../preserved.js'
import {
    applyPlaced, childrenOf, diffBlocks, encodePlaces, encodeTarget, insertBelow, locate,
    nodesKey, resolveRelative, siblingsBetween,
} from './proposalCore.js'

export const FIELD = 'default'

const refuse = (status, message) => Object.assign(new Error(message), { status })

// baseHash is the hash of a target's base: SHA-256, hex, of its content key, so
// it ignores block ids and default attributes as the comparison does.
export const baseHash = (nodes) => createHash('sha256').update(nodesKey(nodes), 'utf8').digest('hex')

// The schema to read this doc with: the editor's, plus a keeper for any node or
// mark it does not know (preserved.js), so reading never drops one.
function schemaFor(fragment, extensions) {
    const unknown = unknownTypes({ type: 'doc', content: yXmlFragmentToProsemirrorJSON(fragment).content || [] }, getSchema(extensions))
    return getSchema(unknown.nodes.size || unknown.marks.size ? [...extensions, ...keepersFor(unknown)] : extensions)
}

// readLive is the doc as ProseMirror sees it, with the map from its Yjs types.
// y-prosemirror deletes an element it cannot read, as a change to the doc; so
// the read is tried on a copy first, and a doc it would change is refused.
function readLive(ydoc, extensions) {
    const fragment = ydoc.getXmlFragment(FIELD)
    const schema = schemaFor(fragment, extensions)
    const probe = new Y.Doc()
    Y.applyUpdate(probe, Y.encodeStateAsUpdate(ydoc))
    let changed = false
    probe.on('update', () => { changed = true })
    initProseMirrorDoc(probe.getXmlFragment(FIELD), schema)
    probe.destroy()
    if (changed) throw refuse(500, 'the doc holds content this service cannot read safely')
    const { doc, meta } = initProseMirrorDoc(fragment, schema)
    return { fragment, schema, doc, meta }
}

const headingText = (node) => node.textContent.trim().toLowerCase()

// findTarget turns a server-side target (from the agent's request) into a run of
// sibling blocks: { from, to }.
export function findTarget(doc, target) {
    const top = childrenOf(doc)
    const starts = []
    let pos = 0
    for (const node of top) {
        starts.push(pos)
        pos += node.nodeSize
    }
    const end = doc.content.size
    if (!target || target.whole) return { from: 0, to: end }
    if (target.append) return { from: end, to: end }
    const byId = (id) => {
        let found = null
        doc.descendants((node, at) => {
            if (found !== null) return false
            if (node.isBlock && node.attrs && node.attrs.blockId === id) {
                found = { from: at, to: at + node.nodeSize }
                return false
            }
            return true
        })
        return found
    }
    if (Array.isArray(target.block_ids) && target.block_ids.length) {
        const places = target.block_ids.map(byId)
        if (places.some((x) => x === null)) throw refuse(404, 'a block it names is not in the doc')
        const from = Math.min(...places.map((x) => x.from))
        const to = Math.max(...places.map((x) => x.to))
        if (!siblingsBetween(doc, from, to)) throw refuse(422, 'the blocks it names are not side by side')
        return { from, to }
    }
    if (typeof target.after_block_id === 'string' && target.after_block_id) {
        const at = byId(target.after_block_id)
        if (!at) throw refuse(404, 'the block it names is not in the doc')
        return { from: at.to, to: at.to }
    }
    const section = typeof target.section === 'string' ? target.section : typeof target.after_section === 'string' ? target.after_section : ''
    if (section) {
        const want = section.trim().toLowerCase()
        const i = top.findIndex((n) => n.type.name === 'heading' && headingText(n) === want)
        if (i < 0) throw refuse(404, 'there is no heading with that text')
        const level = top[i].attrs.level
        let j = i + 1
        while (j < top.length && !(top[j].type.name === 'heading' && top[j].attrs.level <= level)) j++
        const to = j < top.length ? starts[j] : end
        // A section's heading stays; what is under it is the target.
        const from = starts[i] + top[i].nodeSize
        return target.after_section ? { from: to, to } : { from, to }
    }
    throw refuse(422, 'the target names no block, heading or place')
}

const lineageOf = (ydoc) => {
    const meta = ydoc.getMap('meta')
    const v = meta.get('lineage') ?? meta.get('epoch')
    return typeof v === 'string' ? v : ''
}

// prepare makes the hunks for a proposal made on the server.
export function prepareOn(ydoc, proposal, extensions) {
    const pending = proposal.pending || {}
    const { fragment, schema, doc } = readLive(ydoc, extensions)
    const target = findTarget(doc, pending.target)
    const base = siblingsBetween(doc, target.from, target.to)
    if (!base) throw refuse(422, 'the target is not a run of whole blocks')
    const html = typeof pending.html === 'string' ? pending.html.trim() : ''
    if (!html) throw refuse(400, 'nothing is proposed')
    const { json } = htmlToDoc(html, extensions)
    const proposed = (json.content || []).map((n) => schema.nodeFromJSON(n))
    if (proposed.length === 0) throw refuse(400, 'nothing is proposed')
    const hunks = encodePlaces(fragment, doc, target.from, base, diffBlocks(base, proposed))
    if (hunks.length === 0) throw refuse(409, 'the proposal changes nothing')
    const ends = encodeTarget(fragment, doc, target)
    const first = base[0] || doc.resolve(target.from).nodeBefore
    return {
        anchor: {
            kind: 'yrange',
            start: ends.start,
            end: ends.end,
            block_id: first && first.attrs && typeof first.attrs.blockId === 'string' ? first.attrs.blockId : undefined,
            lineage: lineageOf(ydoc) || undefined,
            quote: base.map((b) => b.textContent).join('\n').slice(0, 2000) || undefined,
        },
        base: base.map((b) => b.toJSON()),
        base_hash: baseHash(base),
        proposed: proposed.map((n) => n.toJSON()),
        hunks,
    }
}

// placeOn finds the proposal's hunks in the live doc.
function placeOn(ydoc, live, proposal, ids) {
    const { fragment, doc, meta } = live
    const anchor = proposal.anchor || {}
    const lineage = lineageOf(ydoc)
    const lost = anchor.lineage && lineage && anchor.lineage !== lineage
    const resolve = lost ? null : (e) => resolveRelative(ydoc, fragment, meta.mapping, e)
    const from = resolve ? resolve(anchor.start) : null
    const to = resolve ? resolve(anchor.end) : null
    const range = from !== null && to !== null && from <= to ? { from, to } : null
    return { range, ...locate(doc, range, proposal.base, proposal.hunks || [], ids, resolve) }
}

const conflict = (ids) => Object.assign(refuse(409, JSON.stringify({ conflicts: ids })), { conflicts: ids })

// The top-level blocks a change touched, [from, to), for the record of an
// agent's edit (A4's runWrite keeps it beside the body).
function touched(tr, from, to) {
    const a = tr.doc.resolve(Math.min(tr.mapping.map(from, -1), tr.doc.content.size))
    const b = tr.doc.resolve(Math.min(tr.mapping.map(to, 1), tr.doc.content.size))
    return { from: a.index(0), to: Math.max(a.index(0), b.indexAfter(0)) }
}

// proposalWrite is a step that changes the doc (apply, insert below) as the
// three parts A4's runWrite takes (live/ops/write.js): build, locate (finds
// where it goes and refuses; changes nothing, and runs again inside the write's
// transaction, so the doc cannot move between the check and the write) and
// apply (makes the change inside that transaction).
export function proposalWrite(proposal, mode, ids, extensions) {
    return {
        build: () => proposal,
        locate(doc) {
            const live = readLive(doc, extensions)
            const found = placeOn(doc, live, proposal, mode === 'apply' ? ids : [])
            const tr = new Transform(live.doc)
            if (mode === 'apply') {
                // All or none: a change whose text moved on stops the step.
                if (found.conflicts.length) throw conflict(found.conflicts)
                if (found.placed.length === 0) throw conflict([])
                applyPlaced(tr, found.placed)
                const from = Math.min(...found.placed.map((x) => x.from))
                const to = Math.max(...found.placed.map((x) => x.to))
                return { live, tr, span: touched(tr, from, to), applied: found.placed.map((x) => x.hunk.id) }
            }
            let at = found.range ? found.range.to : null
            const lost = proposal.anchor && proposal.anchor.lineage && lineageOf(doc) && proposal.anchor.lineage !== lineageOf(doc)
            if (at === null && proposal.anchor && proposal.anchor.end && !lost) {
                at = resolveRelative(doc, live.fragment, live.meta.mapping, proposal.anchor.end)
            }
            const where = insertBelow(tr, at ?? live.doc.content.size, proposal.proposed)
            if (where === null) throw refuse(422, 'the proposal has no blocks that fit the doc')
            return { live, tr, span: touched(tr, where, where), applied: [] }
        },
        apply(doc, _built, at) {
            updateYFragment(doc, at.live.fragment, at.tr.doc, at.live.meta)
            return { from: at.span.from, to: at.span.to, blocks: at.span.to - at.span.from, removed: 0, applied: at.applied }
        },
    }
}

// applyOn applies the chosen hunks, all or none, in one transaction.
export function applyOn(ydoc, proposal, ids, extensions, origin) {
    const w = proposalWrite(proposal, 'apply', ids, extensions)
    const at = w.locate(ydoc)
    let result
    ydoc.transact(() => { result = w.apply(ydoc, proposal, at) }, origin)
    return { applied: result.applied, blocks: result.blocks }
}

// insertBelowOn puts the proposed blocks below the target, or where it was, or
// at the end, in one transaction.
export function insertBelowOn(ydoc, proposal, extensions, origin) {
    const w = proposalWrite(proposal, 'insert_below', [], extensions)
    const at = w.locate(ydoc)
    let result
    ydoc.transact(() => { result = w.apply(ydoc, proposal, at) }, origin)
    return { applied: [], blocks: result.blocks }
}

// A4's runWrite (live/ops/write.js), when this build has it: every write's one
// contract (contributors, the body before the change for snapshot_first, an
// agent's edit recorded, an honest "persisted"). Without it (or without the live
// server's deps) the op makes its own transaction, as before A4.
async function runWriteHere() {
    try {
        return (await import('./write.js')).runWrite
    } catch (e) {
        if (e && e.code === 'ERR_MODULE_NOT_FOUND') return null
        throw e
    }
}

// The API, as the op reaches it: GET the proposal, POST what prepare made. Taken
// from the op's arguments when the live server passes them, else from the same
// environment index.js reads.
function apiFrom(args) {
    const base = (args.backendUrl || process.env.GO_BACKEND_URL || 'http://localhost:3000').replace(/\/+$/, '')
    const secret = () => (args.getSecret ? args.getSecret() : (process.env.INTERNAL_SECRET || '').trim())
    const call = async (method, path, body) => {
        const res = await fetch(base + path, {
            method,
            headers: { 'Content-Type': 'application/json', 'X-Internal-Secret': secret() },
            body: body === undefined ? undefined : JSON.stringify(body),
        })
        const data = await res.json().catch(() => ({}))
        if (!res.ok) throw refuse(res.status === 404 ? 404 : 502, data.msg || `the API answered ${res.status}`)
        return data.data
    }
    return {
        load: args.loadProposal || ((id) => call('GET', `/docColab/proposals/${id}`)),
        savePrepared: args.savePrepared || ((id, prepared) => call('POST', `/docColab/proposals/${id}/prepared`, prepared)),
    }
}

const UUID = /^[0-9a-fA-F-]{36}$/

// applyProposal is the op (live/routes.js): async ({ docName, body, hocuspocus }).
// body: { proposal_id, hunk_ids, by: { user_uuid, agent_uuid } } as the API's
// Live Doc Writer client sends it.
export async function applyProposal(args) {
    const { docName, body = {}, hocuspocus } = args
    const extensions = args.extensions || (args.deps && args.deps.extensions) || COLLAB_EXTENSIONS
    const id = typeof body.proposal_id === 'string' ? body.proposal_id : ''
    if (!UUID.test(id)) throw refuse(400, 'proposal_id is required')
    if (!hocuspocus) throw refuse(503, 'the live server is not ready')
    const api = apiFrom(args)
    const proposal = await api.load(id)
    if (!proposal || proposal.id !== id) throw refuse(404, 'no such proposal')
    if ((proposal.resource_uuid || '').toLowerCase() !== docName.toLowerCase()) throw refuse(409, 'the proposal is for another doc')
    const pending = proposal.pending || {}
    const mode = pending.mode
    const ids = Array.isArray(pending.hunk_ids) ? pending.hunk_ids : []
    const asked = Array.isArray(body.hunk_ids) ? body.hunk_ids : []
    if (mode === 'apply' && asked.length && asked.join(',') !== ids.join(',')) throw refuse(409, 'the hunks asked for are not the ones the proposal is waiting on')

    const by = body.by || pending.by || {}
    const user = by.user_uuid || by.agent_uuid || 'unknown'
    if (mode === 'prepare') {
        const connection = await hocuspocus.openDirectConnection(docName.toLowerCase(), { user: { id: user, name: 'Edit proposal' } })
        let prepared
        try {
            // A read: no transaction, so no save of an unchanged doc.
            prepared = prepareOn(connection.document, proposal, extensions)
        } finally {
            await connection.disconnect()
        }
        await api.savePrepared(id, prepared)
        return { blocks: 0, prepared: true }
    }
    if (mode !== 'apply' && mode !== 'insert_below') throw refuse(409, 'the proposal is not waiting on a step')

    const deps = args.deps || {}
    const runWrite = args.runWrite || (deps.contributors && deps.persistence ? await runWriteHere() : null)
    if (runWrite) {
        return runWrite({ docName, body, hocuspocus, deps }, proposalWrite(proposal, mode, ids, extensions))
    }
    // The origin carries who it is for, so the save hook credits them (onChange
    // reads context.user from it).
    const origin = { context: { user: { id: user } }, proposal: id }
    const connection = await hocuspocus.openDirectConnection(docName.toLowerCase(), { user: { id: user, name: 'Edit proposal' } })
    let result
    try {
        await connection.transact((ydoc) => {
            result = mode === 'apply' ? applyOn(ydoc, proposal, ids, extensions, origin) : insertBelowOn(ydoc, proposal, extensions, origin)
        })
    } finally {
        await connection.disconnect()
    }
    return { ...result, persisted_at: new Date().toISOString() }
}
