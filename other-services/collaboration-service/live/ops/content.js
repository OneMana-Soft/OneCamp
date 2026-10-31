// The Live Doc Writer's content ops (live/routes.js). Each is one Yjs
// transaction on the live doc, run by runWrite (write.js); here is only what
// each one changes.

import { FIELD, blockKey, htmlToBlocks, indexOfBlock, isBlank } from './blocks.js'
import { MAX_BLOCK_IDS, htmlOf, refuse, runWrite } from './write.js'

const fragmentOf = (doc) => doc.getXmlFragment(FIELD)

// blocksFor builds the blocks an op writes; an op that must write something
// refuses HTML that holds nothing.
function blocksFor(html, extensions, { allowNone = false } = {}) {
    const blocks = html ? htmlToBlocks(html, extensions) : []
    if (!blocks.length && !allowNone) throw refuse(400, 'nothing to write')
    return blocks
}

// insert writes blocks at `at` and says what it wrote.
export function insert(doc, blocks, at, removed = 0) {
    if (blocks.length) fragmentOf(doc).insert(at, blocks.map((b) => b.node))
    return { from: at, to: at + blocks.length, blocks: blocks.length, removed, blockIds: blocks.map((b) => b.id).filter(Boolean) }
}

// append adds blocks at the end of the body.
export async function append(ctx) {
    const html = htmlOf(ctx.body)
    return runWrite(ctx, {
        build: () => blocksFor(html, ctx.deps.extensions),
        locate: (doc) => fragmentOf(doc).length,
        apply: (doc, blocks, at) => insert(doc, blocks, at),
    })
}

// insertAfter adds blocks after the block with that id.
export async function insertAfter(ctx) {
    const html = htmlOf(ctx.body)
    const id = typeof ctx.body?.block_id === 'string' ? ctx.body.block_id : ''
    if (!id) throw refuse(400, 'block_id is required')
    return runWrite(ctx, {
        build: () => blocksFor(html, ctx.deps.extensions),
        locate: (doc) => {
            const i = indexOfBlock(fragmentOf(doc), id)
            if (i < 0) throw refuse(404, 'no such block')
            return i + 1
        },
        apply: (doc, blocks, at) => insert(doc, blocks, at),
    })
}

// replaceBlocks replaces the blocks with those ids by the new ones, written
// where the first of them was. Empty HTML takes them away.
export async function replaceBlocks(ctx) {
    const html = htmlOf(ctx.body, { required: false })
    const ids = Array.isArray(ctx.body?.block_ids) ? [...new Set(ctx.body.block_ids.filter((x) => typeof x === 'string' && x))] : []
    if (!ids.length) throw refuse(400, 'block_ids is required')
    if (ids.length > MAX_BLOCK_IDS) throw refuse(400, `at most ${MAX_BLOCK_IDS} blocks at once`)
    return runWrite(ctx, {
        build: () => blocksFor(html, ctx.deps.extensions, { allowNone: true }),
        locate: (doc) => {
            const fragment = fragmentOf(doc)
            const at = ids.map((id) => indexOfBlock(fragment, id))
            if (at.some((i) => i < 0)) throw refuse(404, 'no such block')
            return at.sort((a, b) => a - b)
        },
        apply: (doc, blocks, at) => {
            const fragment = fragmentOf(doc)
            for (let k = at.length - 1; k >= 0; k--) fragment.delete(at[k], 1)
            return insert(doc, blocks, at[0], at.length)
        },
    })
}

// replaceAll makes the body the given HTML, as a restore or an API write does.
// Blocks that read the same at the start and the end are kept as they are, so
// their identity, and every comment anchored in them, survives; only the
// stretch between is replaced. Empty HTML empties the doc.
export async function replaceAll(ctx) {
    const html = htmlOf(ctx.body, { required: false })
    return runWrite(ctx, {
        build: () => blocksFor(html || '<p></p>', ctx.deps.extensions),
        locate: (doc, blocks) => {
            const old = fragmentOf(doc).toArray().map(blockKey)
            const neu = blocks.map((b) => b.key)
            let head = 0
            while (head < old.length && head < neu.length && old[head] === neu[head]) head++
            let tail = 0
            while (tail < old.length - head && tail < neu.length - head && old[old.length - 1 - tail] === neu[neu.length - 1 - tail]) tail++
            return { head, tail, oldLength: old.length }
        },
        apply: (doc, blocks, { head, tail, oldLength }) => {
            const removed = oldLength - head - tail
            if (removed > 0) fragmentOf(doc).delete(head, removed)
            return insert(doc, blocks.slice(head, blocks.length - tail), head, removed)
        },
    })
}

// seed writes the first body of a doc nobody has written in yet: a template, a
// duplicate, an import, the demo. A doc that already holds anything is refused
// (409), so two openers, or a retry, never write it twice.
export async function seed(ctx) {
    const html = htmlOf(ctx.body)
    return runWrite(ctx, {
        build: () => blocksFor(html, ctx.deps.extensions),
        locate: (doc) => {
            const fragment = fragmentOf(doc)
            if (!isBlank(fragment)) throw refuse(409, 'the doc already has content')
            return fragment.length
        },
        apply: (doc, blocks, blank) => {
            if (blank > 0) fragmentOf(doc).delete(0, blank)
            return insert(doc, blocks, 0, blank)
        },
    })
}
