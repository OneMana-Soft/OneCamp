// A highlight (docs programme B2; the web's extensions/highlight): a mark by
// colour name, <mark data-color="sky">, yellow when none is named.
import { Mark } from '@tiptap/core'
import { isDocColour } from './colours.js'

const colourOf = (v) => (isDocColour(v) ? v : 'sun')

export const HighlightSchema = Mark.create({
    name: 'highlight',
    addAttributes() {
        return {
            color: {
                default: 'sun',
                parseHTML: (el) => colourOf(el.getAttribute('data-color')),
                renderHTML: (attrs) => ({ 'data-color': colourOf(attrs.color) }),
            },
        }
    },
    parseHTML() {
        return [{ tag: 'mark' }]
    },
    renderHTML({ HTMLAttributes }) {
        return ['mark', HTMLAttributes, 0]
    },
})
