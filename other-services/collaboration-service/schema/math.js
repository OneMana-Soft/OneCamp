// Equations (docs programme B3): inlineMath and blockMath, holding only their
// TeX (latex), with Tiptap 3's names and HTML (span or div with data-type
// "inline-math" or "block-math", and data-latex), as the web's
// extensions/math/math.ts writes them. The TeX is the element's text too, so
// the saved body reads as the equation's source wherever it's shown as HTML.
import { Node, mergeAttributes } from '@tiptap/core'

const latex = {
    latex: {
        default: '',
        parseHTML: (el) => el.getAttribute('data-latex') ?? '',
        renderHTML: (attrs) => ({ 'data-latex': attrs.latex ?? '' }),
    },
}

export const InlineMathSchema = Node.create({
    name: 'inlineMath',
    group: 'inline',
    inline: true,
    atom: true,
    addAttributes: () => latex,
    parseHTML() {
        return [{ tag: 'span[data-type="inline-math"]' }]
    },
    renderHTML({ node, HTMLAttributes }) {
        return ['span', mergeAttributes(HTMLAttributes, { 'data-type': 'inline-math' }), node.attrs.latex ?? '']
    },
})

export const BlockMathSchema = Node.create({
    name: 'blockMath',
    group: 'block',
    atom: true,
    addAttributes: () => latex,
    parseHTML() {
        return [{ tag: 'div[data-type="block-math"]' }]
    },
    renderHTML({ node, HTMLAttributes }) {
        return ['div', mergeAttributes(HTMLAttributes, { 'data-type': 'block-math' }), node.attrs.latex ?? '']
    },
})
