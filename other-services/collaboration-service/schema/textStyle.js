// Text colour by name (docs programme B2, N15): an attribute of the textStyle
// mark, written <span data-color="sky">, as the web's extensions/color does.
// An old doc's style="color: var(--camp-sky-ink)" reads as "sky"; another
// colour is dropped, as the web drops it.
import { TextStyle } from '@tiptap/extension-text-style'
import { Color } from '@tiptap/extension-color'
import { colourName, styleColor } from './colours.js'

const colourOf = (el) => colourName(el.getAttribute('data-color') || styleColor(el))

// A span is coloured text only in one of our colours: no empty marks.
export const TextStyleSchema = TextStyle.extend({
    parseHTML() {
        return [{ tag: 'span', consuming: false, getAttrs: (el) => (colourOf(el) ? {} : false) }]
    },
})

export const ColorSchema = Color.extend({
    addGlobalAttributes() {
        return [
            {
                types: this.options.types,
                attributes: {
                    color: {
                        default: null,
                        parseHTML: colourOf,
                        renderHTML: ({ color }) => (colourName(color) ? { 'data-color': colourName(color) } : {}),
                    },
                },
            },
        ]
    },
})
