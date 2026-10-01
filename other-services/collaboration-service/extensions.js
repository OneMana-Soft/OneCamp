// The editor's schema, shared by the server and anything that converts HTML
// to and from the collaborative document. It MUST match the frontend exactly:
// a missing extension loses that content on the next save.
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
    Image,
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
]
