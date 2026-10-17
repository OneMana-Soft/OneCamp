// A doc holding every node and mark the web editor writes, each attribute set
// to a value that is not its default, so a round trip that drops one shows.
// schemaParity.test.js checks it covers every type in schema-manifest.json.
import { INDENT_NODES } from './schema/indent.js'
import { DIR_NODES } from './schema/blockDir.js'

const t = (text, marks) => (marks ? { type: 'text', text, marks } : { type: 'text', text })
const p = (...content) => ({ type: 'paragraph', content })

// Every block carries its id, as a doc's blocks do (schema/blockId.js): set
// below, on each node that is not inline.
const INLINE = new Set(['doc', 'text', 'hardBreak', 'mention', 'channelMention', 'entityMention', 'inlineMath', 'linkMention', 'dateMention'])
const withBlockIds = (node, next = { n: 0 }) => ({
    ...node,
    ...(INLINE.has(node.type) ? {} : { attrs: { ...(node.attrs || {}), blockId: `blk-${++next.n}` } }),
    ...(node.content ? { content: node.content.map((c) => withBlockIds(c, next)) } : {}),
})

// B1's indent and direction, on every block that takes them (schema/indent.js,
// schema/blockDir.js): neither is ever its default here.
const withLayout = (node) => ({
    ...node,
    ...(INDENT_NODES.includes(node.type) || DIR_NODES.includes(node.type)
        ? { attrs: { ...(node.attrs || {}), ...(INDENT_NODES.includes(node.type) ? { indent: 2 } : {}), ...(DIR_NODES.includes(node.type) ? { dir: 'rtl' } : {}) } }
        : {}),
    ...(node.content ? { content: node.content.map(withLayout) } : {}),
})

export const EVERY_BLOCK = withBlockIds(withLayout({
    type: 'doc',
    content: [
        { type: 'heading', attrs: { level: 2, blockColor: 'berry' }, content: [t('Launch plan')] },
        p(
            t('Bold', [{ type: 'bold' }]), t(' '),
            t('italic', [{ type: 'italic' }]), t(' '),
            t('struck', [{ type: 'strike' }]), t(' '),
            t('code', [{ type: 'code' }]), t(' '),
            t('under', [{ type: 'underline' }]), t(' '),
            t('blue', [{ type: 'textStyle', attrs: { color: 'sky' } }]), t(' '),
            t('highlighted', [{ type: 'highlight', attrs: { color: 'lake' } }]), t(' '),
            t('a link', [{ type: 'link', attrs: { href: 'https://onemana.dev/x', target: '_blank', rel: 'noopener noreferrer nofollow', class: 'link' } }]),
            { type: 'hardBreak' },
            t('Ask '),
            { type: 'mention', attrs: { id: 'u-1', label: 'Sam Rivera', mentionSuggestionChar: '@' } },
            t(' in '),
            { type: 'channelMention', attrs: { id: 'c-1', label: 'engineering', mentionSuggestionChar: '#' } },
            t(' about '),
            { type: 'entityMention', attrs: { id: 'doc:d-1', label: 'Launch notes', mentionSuggestionChar: '+' } },
            t(' and '),
            { type: 'linkMention', attrs: { href: 'https://onemana.dev/blog', title: 'The blog', icon: 'https://x/icon.png', siteName: 'OneMana' } },
            t(' by '),
            { type: 'dateMention', attrs: { date: '2026-10-12', reminder: 'later-1' } },
        ),
        { type: 'paragraph', attrs: { blockColor: 'sky_background' }, content: [t('A blue line.')] },
        { type: 'blockquote', attrs: { blockColor: 'gray_background', quoteSize: 'large' }, content: [p(t('Quoted.'))] },
        { type: 'bulletList', content: [{ type: 'listItem', attrs: { blockColor: 'moss' }, content: [p(t('One'))] }] },
        { type: 'orderedList', attrs: { start: 3, type: 'a' }, content: [{ type: 'listItem', content: [p(t('Three'))] }] },
        { type: 'taskList', content: [{ type: 'taskItem', attrs: { checked: true, blockColor: 'dusk_background', taskId: '3a3f4d5e-6f7a-4b8c-9d2e-3f4a5b6c7d8e' }, content: [p(t('Done'))] }] },
        { type: 'horizontalRule' },
        { type: 'codeBlock', attrs: { language: 'go', wrap: true, caption: 'Says hi' }, content: [t('fmt.Println("hi")')] },
        // B3: an equation in a line and one on its own, and a simple table:
        // a header cell spanning two columns with their widths, a coloured cell.
        p(t('Energy is '), { type: 'inlineMath', attrs: { latex: 'E = mc^2' } }, t('.')),
        { type: 'blockMath', attrs: { latex: '\\int_0^1 x\\,dx' } },
        {
            type: 'table',
            content: [
                {
                    type: 'tableRow',
                    content: [{ type: 'tableHeader', attrs: { colspan: 2, rowspan: 1, colwidth: [120, 80], background: 'sky' }, content: [p(t('Plan'))] }],
                },
                {
                    type: 'tableRow',
                    content: [
                        { type: 'tableCell', attrs: { colspan: 1, rowspan: 1, colwidth: [120], background: 'moss' }, content: [p(t('Ship'))] },
                        { type: 'tableCell', attrs: { colspan: 1, rowspan: 1, colwidth: [80], background: 'berry' }, content: [p(t('Friday'))] },
                    ],
                },
            ],
        },
        {
            type: 'image',
            attrs: { src: 'https://onemana.dev/a.png', alt: 'A chart', title: 'Chart', id: 'img-1', width: 300, height: 200, caption: 'Revenue by month', fileName: 'chart.png', align: 'left', crop: '0.1,0.1,0.8,0.8', href: 'https://onemana.dev/' },
        },
        { type: 'callout', attrs: { emoji: '🚀', color: 'green', icon: 'rocket' }, content: [p(t('Ship it.'))] },
        { type: 'collapsible', attrs: { blockColor: 'sun_background' }, content: [p(t('Details')), p(t('Hidden.'))] },
        { type: 'collapsible', content: [{ type: 'heading', attrs: { level: 3 }, content: [t('A toggle heading')] }, p(t('Under it.'))] },
        { type: 'toc' },
        { type: 'tableEmbed', attrs: { tableId: 'tbl-1', viewId: 'view-1' } },
        { type: 'poll', attrs: { id: 'poll-1' } },
        { type: 'chartEmbed', attrs: { spec: '{"type":"bar","data":[1,2]}' } },
        { type: 'diffEmbed', attrs: { diff: '--- a\n+++ b\n@@ -1 +1 @@\n-x\n+y' } },
        { type: 'recordingEmbed', attrs: { egress: 'eg-1', kind: 'audio', sid: 'room-1', u1: 'https://x/1', u2: 'https://x/2' } },
        // B4: links and media.
        { type: 'bookmark', attrs: { href: 'https://onemana.dev/blog', title: 'The blog', description: 'What we shipped', siteName: 'OneMana', icon: 'https://x/icon.png', image: 'https://x/card.png' } },
        { type: 'webEmbed', attrs: { src: 'https://www.youtube.com/watch?v=dQw4w9WgXcQ', height: 360 } },
        { type: 'fileBlock', attrs: { kind: 'pdf', src: 'https://x/plan.pdf', id: 'up-1-abc', name: 'plan.pdf', size: 2048, mime: 'application/pdf', caption: 'The plan' } },
        {
            type: 'columnList',
            content: [
                { type: 'column', attrs: { width: 40 }, content: [p(t('Left'))] },
                { type: 'column', attrs: { width: 60 }, content: [p(t('Right'))] },
            ],
        },
        // C1: a page, a project's tasks and board.
        { type: 'subpage', attrs: { docId: '0d0c1a2b-3c4d-4e5f-8a9b-0c1d2e3f4a5b', title: 'Launch plan' } },
        { type: 'taskListEmbed', attrs: { projectId: '2f2e3c4d-5e6f-4a7b-8c1d-2e3f4a5b6c7d', show: 'mine' } },
        { type: 'boardEmbed', attrs: { projectId: '2f2e3c4d-5e6f-4a7b-8c1d-2e3f4a5b6c7d' } },
    ],
}))

// The type names a doc uses.
export function typesIn(json) {
    const nodes = new Set()
    const marks = new Set()
    const visit = (n) => {
        nodes.add(n.type)
        for (const m of n.marks || []) marks.add(m.type)
        for (const c of n.content || []) visit(c)
    }
    visit(json)
    return { nodes, marks }
}
