// Sub-pages in step with the tree (docs C7a): a page's block follows the page
// as the tree moves it or brings it back from the trash (business/Doc
// FollowMove, FollowRestore), and a page made under a page takes one at its
// end (PlaceInParent). A sub-page block is the `subpage` node naming its page
// by docId (the web's extensions/subpage). "/link to page" places the same
// node, so a page's blocks are every subpage node that names it, wherever the
// editor nests it: replaceBlocks reaches only top-level blocks, by their ids.
//
// body.doc_id is the page. Each op is one transaction, like every op, and
// refuses what would change nothing before anyone is credited with a change
// (runWrite): removeSubpage a doc that holds no block of the page (404), and
// placeSubpage one that holds one already (409), so a retry, or a parent
// whose link to the page was placed earlier, never takes a second.

import { idOf } from '../../schema/connected.js'
import { FIELD, elementsNamed, htmlToBlocks } from './blocks.js'
import { insert } from './content.js'
import { refuse, runWrite } from './write.js'

const pageOf = (body) => {
    const id = idOf(body?.doc_id)
    if (!id) throw refuse(400, 'doc_id is required: the page')
    return id
}

// blocksOf is every sub-page block of the page in the body, at any depth.
export function blocksOf(fragment, page) {
    return elementsNamed(fragment, 'subpage').filter(({ node }) => idOf(node.getAttribute('docId')) === page)
}

// dropBlocks takes the blocks found out of the body, and says how many went.
// A container they leave empty (a callout, a column) keeps an empty line, as
// does a body they leave blank: a container with no block is outside the
// editor's schema, and an editor drops it whole (y-prosemirror), a callout's
// icon with it. filler makes each line.
export function dropBlocks(found, filler) {
    const byParent = new Map()
    for (const { parent, index } of found) byParent.set(parent, [...(byParent.get(parent) || []), index])
    for (const [parent, indexes] of byParent) {
        for (const i of indexes.sort((a, b) => b - a)) parent.delete(i, 1)
        if (parent.length === 0) parent.insert(0, [filler()])
    }
    return found.length
}

// removeSubpage takes every block of the page out of the doc: the page left it.
export async function removeSubpage(ctx) {
    const page = pageOf(ctx.body)
    return runWrite(ctx, {
        build: () => () => htmlToBlocks('<p></p>', ctx.deps.extensions)[0].node,
        locate: (doc) => {
            const found = blocksOf(doc.getXmlFragment(FIELD), page)
            if (!found.length) throw refuse(404, 'no block of that page')
            return found
        },
        apply: (doc, filler, found) => ({ from: 0, to: 0, blocks: 0, removed: dropBlocks(found, filler) }),
    })
}

// placeSubpage puts the page's block at the end of the doc: the page is in it.
export async function placeSubpage(ctx) {
    const page = pageOf(ctx.body)
    return runWrite(ctx, {
        build: () => htmlToBlocks(`<div data-type="subpage" data-doc-id="${page}"></div>`, ctx.deps.extensions),
        locate: (doc) => {
            const fragment = doc.getXmlFragment(FIELD)
            if (blocksOf(fragment, page).length) throw refuse(409, 'the doc holds a block of that page already')
            return fragment.length
        },
        apply: (doc, blocks, at) => insert(doc, blocks, at),
    })
}
