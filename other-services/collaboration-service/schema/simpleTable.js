// The simple table (docs programme B3): Tiptap's node names, attributes and
// HTML, as the web's extensions/simple-table/simpleTable.ts writes them. A
// cell's colour is stored by name (data-background="sky"). Editing (cell
// selection, resizing) is the browser's; here the nodes only need to load and
// save, so prosemirror-tables isn't needed.
import { Node, mergeAttributes } from '@tiptap/core'
import { isDocColour } from './colours.js'

const cellAttributes = {
    colspan: { default: 1 },
    rowspan: { default: 1 },
    colwidth: {
        default: null,
        parseHTML: (el) => {
            const w = el.getAttribute('colwidth')
            const widths = w ? w.split(',').map((x) => parseInt(x, 10)) : null
            return widths && widths.every((x) => x > 0) ? widths : null
        },
    },
    background: {
        default: null,
        parseHTML: (el) => {
            const v = el.getAttribute('data-background')
            return isDocColour(v) ? v : null
        },
        renderHTML: (attrs) => (attrs.background ? { 'data-background': attrs.background } : {}),
    },
}

export const TableSchema = Node.create({
    name: 'table',
    group: 'block',
    content: 'tableRow+',
    isolating: true,
    parseHTML() {
        return [{ tag: 'table' }]
    },
    renderHTML({ node, HTMLAttributes }) {
        // The columns' widths travel with the HTML, as the web writes them.
        const cols = []
        const row = node.firstChild
        for (let i = 0; row && i < row.childCount; i++) {
            const { colspan, colwidth } = row.child(i).attrs
            for (let j = 0; j < colspan; j++) cols.push(['col', colwidth?.[j] ? { style: `width: ${colwidth[j]}px` } : {}])
        }
        return ['table', mergeAttributes(HTMLAttributes), ['colgroup', {}, ...cols], ['tbody', 0]]
    },
})

export const TableRowSchema = Node.create({
    name: 'tableRow',
    content: '(tableCell | tableHeader)*',
    parseHTML() {
        return [{ tag: 'tr' }]
    },
    renderHTML({ HTMLAttributes }) {
        return ['tr', mergeAttributes(HTMLAttributes), 0]
    },
})

export const TableHeaderSchema = Node.create({
    name: 'tableHeader',
    content: 'block+',
    isolating: true,
    addAttributes: () => cellAttributes,
    parseHTML() {
        return [{ tag: 'th' }]
    },
    renderHTML({ HTMLAttributes }) {
        return ['th', mergeAttributes(HTMLAttributes), 0]
    },
})

export const TableCellSchema = Node.create({
    name: 'tableCell',
    content: 'block+',
    isolating: true,
    addAttributes: () => cellAttributes,
    parseHTML() {
        return [{ tag: 'td' }]
    },
    renderHTML({ HTMLAttributes }) {
        return ['td', mergeAttributes(HTMLAttributes), 0]
    },
})
