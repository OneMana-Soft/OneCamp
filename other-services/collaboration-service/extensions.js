// The editor's schema, shared by the server and anything that converts HTML
// to and from the collaborative document. It MUST match the frontend exactly:
// a node the server does not know made every save of a doc holding it fail
// ("Unknown node type: tableEmbed"), and an attribute it does not know is
// dropped from the saved body (an image's caption was).
//
// schema-manifest.json lists every node and mark the web editor writes, with
// its attributes, and schemaParity.test.js holds this list to it. The web repo
// keeps a copy of the manifest and tests its editor against it, so a block
// added on one side only fails a test on that side.
import { StarterKit } from '@tiptap/starter-kit'
import { Image } from '@tiptap/extension-image'
import { TextStyle } from '@tiptap/extension-text-style'
import { Color } from '@tiptap/extension-color'
import { Mention } from '@tiptap/extension-mention'
import { Typography } from '@tiptap/extension-typography'
import { TaskList } from '@tiptap/extension-task-list'
import { TaskItem } from '@tiptap/extension-task-item'
import { CodeBlockLowlight } from '@tiptap/extension-code-block-lowlight'
import { Link } from '@tiptap/extension-link'
import { Underline } from '@tiptap/extension-underline'
import { common, createLowlight } from 'lowlight'
import { Node, mergeAttributes } from '@tiptap/core'

const lowlight = createLowlight(common)

// ---------------------------------------------------------------------------
// Minimal node schemas for custom blocks (no React — pure schema for HTML
// round-trip in the collaboration service)
// ---------------------------------------------------------------------------

const Callout = Node.create({
    name: 'callout',
    group: 'block',
    content: 'block+',
    defining: true,
    addAttributes() {
        return {
            emoji: { default: '💡', parseHTML: el => el.getAttribute('data-emoji') || '💡', renderHTML: attrs => attrs.emoji ? { 'data-emoji': attrs.emoji } : {} },
            color: { default: 'blue', parseHTML: el => el.getAttribute('data-color') || 'blue', renderHTML: attrs => attrs.color ? { 'data-color': attrs.color } : {} },
        }
    },
    parseHTML() { return [{ tag: 'div[data-type="callout"]' }] },
    renderHTML({ HTMLAttributes }) {
        return ['div', mergeAttributes({ 'data-type': 'callout' }, HTMLAttributes), 0]
    },
})

const Collapsible = Node.create({
    name: 'collapsible',
    group: 'block',
    content: 'block+',
    defining: true,
    addAttributes() {
        return {
            title: { default: 'Toggle', parseHTML: el => el.getAttribute('data-title') || 'Toggle', renderHTML: attrs => attrs.title ? { 'data-title': attrs.title } : {} },
            open: { default: true, parseHTML: el => el.getAttribute('data-open') !== 'false', renderHTML: attrs => ({ 'data-open': String(attrs.open) }) },
        }
    },
    parseHTML() { return [{ tag: 'div[data-type="collapsible"]' }] },
    renderHTML({ HTMLAttributes }) {
        return ['div', mergeAttributes({ 'data-type': 'collapsible' }, HTMLAttributes), 0]
    },
})

// A data-* attribute that round-trips a string, as the web editor writes it.
const dataAttr = (name, key, fallback = '') => ({
    default: fallback,
    parseHTML: el => el.getAttribute(key) || fallback,
    renderHTML: attrs => (attrs[name] ? { [key]: attrs[name] } : {}),
})

// A read-only block the web editor draws itself (a React node view); here only
// its attributes need to survive. Same name, attributes and HTML as the web's
// extensions/<name>/<name>.ts.
const embed = (name, dataType, attrs) => Node.create({
    name,
    group: 'block',
    atom: true,
    selectable: true,
    draggable: name === 'tableEmbed',
    addAttributes() {
        return Object.fromEntries(Object.entries(attrs).map(([attr, key]) => [attr, dataAttr(attr, key)]))
    },
    parseHTML() { return [{ tag: `div[data-type="${dataType}"]` }] },
    renderHTML({ HTMLAttributes }) {
        return ['div', mergeAttributes({ 'data-type': dataType }, HTMLAttributes)]
    },
})

// `/table`: a live view of a table (extensions/table-embed).
const TableEmbed = embed('tableEmbed', 'table-embed', { tableId: 'data-table-id', viewId: 'data-view-id' })
// A poll card (extensions/poll).
const Poll = embed('poll', 'poll', { id: 'data-id' })
// An agent's chart, diff and meeting recording blocks.
const ChartEmbed = embed('chartEmbed', 'chart', { spec: 'data-spec' })
const DiffEmbed = embed('diffEmbed', 'diff', { diff: 'data-diff' })
const RecordingEmbed = embed('recordingEmbed', 'recording-embed', {
    egress: 'data-egress', kind: 'data-kind', sid: 'data-sid', u1: 'data-u1', u2: 'data-u2',
})

// `#channel` and `+doc` chips: Mention under another name on the web
// (use-minimal-tiptap.ts), written as <span data-type="<name>">.
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
const ChannelMention = referenceMention('channelMention', '#', 'channel-mention')
const EntityMention = referenceMention('entityMention', '', 'entity-mention')

// The web's image (extensions/image/image.ts) is an atom with an id, a caption
// and the uploaded file's name besides Tiptap's own attributes.
const DocImage = Image.extend({
    atom: true,
    addAttributes() {
        return {
            ...this.parent?.(),
            id: { default: null },
            caption: { default: null },
            fileName: { default: null },
        }
    },
})

// ---------------------------------------------------------------------------
// CRITICAL: Extensions MUST match the frontend exactly for lossless
// HTML <-> Yjs round-trips. Missing extensions cause data loss on save/load.
// ---------------------------------------------------------------------------
export const COLLAB_EXTENSIONS = [
    StarterKit.configure({
        codeBlock: false, // Replaced by CodeBlockLowlight on FE
        history: false,   // Yjs handles history in collab mode
        // Tiptap v3's StarterKit bundles Link and Underline. We disable them here
        // and register the standalone versions below with explicit config so the
        // schema matches the frontend and we don't double-register (which logged
        // "Duplicate extension names found: ['link','underline']" and could let a
        // second config silently override the first, diverging the server schema
        // from the client's).
        link: false,
        underline: false,
    }),
    Link.configure({
        openOnClick: false,
        autolink: true,
        defaultProtocol: 'https',
    }),
    Underline,
    DocImage,
    TextStyle,
    Color,
    Mention,
    Typography,
    TaskList.configure({
        HTMLAttributes: { class: 'task-list' },
    }),
    TaskItem.configure({
        HTMLAttributes: { class: 'task-item' },
        nested: true,
    }),
    CodeBlockLowlight.configure({ lowlight }),
    Callout,
    Collapsible,
    TableEmbed,
    Poll,
    ChartEmbed,
    DiffEmbed,
    RecordingEmbed,
    ChannelMention,
    EntityMention,
]
