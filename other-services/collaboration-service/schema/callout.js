// A callout (the web's extensions/callout): its kind (a colour) and an icon
// chosen by its Lucide name, around blocks. Isolating, as on the web (B2, N7):
// Turn into never lifts a line out of it.
import { Node, mergeAttributes } from '@tiptap/core'

// A Lucide name only, as the codec's calloutIcon (business/ContentCodec/doc_format.go).
const LUCIDE_NAME = /^[a-z0-9]+(?:-[a-z0-9]+){0,3}$/
const calloutIcon = (v) => (typeof v === 'string' && v.length <= 40 && LUCIDE_NAME.test(v) ? v : null)

export const CalloutSchema = Node.create({
    name: 'callout',
    group: 'block',
    content: 'block+',
    defining: true,
    isolating: true,
    addAttributes() {
        return {
            emoji: { default: '💡', parseHTML: el => el.getAttribute('data-emoji') || '💡', renderHTML: attrs => attrs.emoji ? { 'data-emoji': attrs.emoji } : {} },
            icon: { default: null, parseHTML: el => calloutIcon(el.getAttribute('data-icon')), renderHTML: attrs => (attrs.icon ? { 'data-icon': attrs.icon } : {}) },
            color: { default: 'blue', parseHTML: el => el.getAttribute('data-color') || 'blue', renderHTML: attrs => attrs.color ? { 'data-color': attrs.color } : {} },
        }
    },
    parseHTML() { return [{ tag: 'div[data-type="callout"]' }] },
    renderHTML({ HTMLAttributes }) {
        return ['div', mergeAttributes({ 'data-type': 'callout' }, HTMLAttributes), 0]
    },
})
