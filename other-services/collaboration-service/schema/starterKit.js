// StarterKit's nodes and marks: the doc, text, paragraphs, headings, quotes,
// lists, line breaks, dividers, bold, italic, strikethrough and inline code.
// The web editor writes the same through its block specs (components/
// minimal-tiptap/blocks/text.ts), in one StarterKit of its own.
import { StarterKit } from '@tiptap/starter-kit'

export const StarterKitSchema = StarterKit.configure({
    codeBlock: false, // codeBlock.js, as the web's own code block
    history: false, // Yjs keeps the history in collaboration
    // Tiptap v3's StarterKit bundles Link and Underline. They are off here and
    // registered on their own (link.js, underline.js) with explicit config, so
    // the schema matches the web's and nothing is registered twice (which
    // logged "Duplicate extension names found: ['link','underline']" and could
    // let a second config silently override the first).
    link: false,
    underline: false,
})
