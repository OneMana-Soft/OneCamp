// Columns (B1; the web's extensions/columns): a row of 2–4 columns, each
// holding blocks, a whole-percent width or an equal share (null). The schema
// is permissive (column+, block+), as the web's is: a change that leaves a
// row with one column still fits, and is never thrown away for not fitting.
import { Node, mergeAttributes } from '@tiptap/core'

const widthOf = (raw) => {
    if (raw == null || !/^\s*\d+\s*$/.test(raw)) return null
    const n = parseInt(raw, 10)
    return n >= 1 && n <= 99 ? n : null
}

export const ColumnListSchema = Node.create({
    name: 'columnList',
    group: 'block',
    content: 'column+',
    isolating: true,
    defining: true,
    parseHTML() {
        return [{ tag: 'div[data-type="column-list"]' }]
    },
    renderHTML({ HTMLAttributes }) {
        return ['div', mergeAttributes({ 'data-type': 'column-list' }, HTMLAttributes), 0]
    },
})

export const ColumnSchema = Node.create({
    name: 'column',
    content: 'block+',
    isolating: true,
    defining: true,
    addAttributes() {
        return {
            width: {
                default: null,
                parseHTML: (el) => widthOf(el.getAttribute('data-width')),
                renderHTML: (attrs) => (typeof attrs.width === 'number' ? { 'data-width': String(attrs.width) } : {}),
            },
        }
    },
    parseHTML() {
        return [{ tag: 'div[data-type="column"]' }]
    },
    renderHTML({ HTMLAttributes }) {
        return ['div', mergeAttributes({ 'data-type': 'column' }, HTMLAttributes), 0]
    },
})
