// The three chips: @person (Tiptap's Mention), and `#channel` and `+doc`,
// Mention under another name on the web (blocks/inline.ts), written as
// <span data-type="<name>">.
import { Node, mergeAttributes } from '@tiptap/core'
import { Mention } from '@tiptap/extension-mention'

export const MentionSchema = Mention

const referenceMention = (name, prefix, className) => Node.create({
    name,
    group: 'inline',
    inline: true,
    selectable: false,
    atom: true,
    addAttributes() {
        return {
            id: { default: null, parseHTML: el => el.getAttribute('data-id'), renderHTML: a => (a.id ? { 'data-id': a.id } : {}) },
            label: { default: null, parseHTML: el => el.getAttribute('data-label'), renderHTML: a => (a.label ? { 'data-label': a.label } : {}) },
            mentionSuggestionChar: {
                default: '@',
                parseHTML: el => el.getAttribute('data-mention-suggestion-char') || '@',
                renderHTML: a => ({ 'data-mention-suggestion-char': a.mentionSuggestionChar }),
            },
        }
    },
    parseHTML() { return [{ tag: `span[data-type="${name}"]` }] },
    renderHTML({ node, HTMLAttributes }) {
        return ['span', mergeAttributes({ 'data-type': name, class: className }, HTMLAttributes), `${prefix}${node.attrs.label ?? node.attrs.id ?? ''}`]
    },
    renderText({ node }) { return `${prefix}${node.attrs.label ?? node.attrs.id ?? ''}` },
})

export const ChannelMentionSchema = referenceMention('channelMention', '#', 'channel-mention')
export const EntityMentionSchema = referenceMention('entityMention', '', 'entity-mention')
