// A doc holding every node and mark the web editor writes, each attribute set
// to a value that is not its default, so a round trip that drops one shows.
// schemaParity.test.js checks it covers every type in schema-manifest.json.

const t = (text, marks) => (marks ? { type: 'text', text, marks } : { type: 'text', text })
const p = (...content) => ({ type: 'paragraph', content })

export const EVERY_BLOCK = {
    type: 'doc',
    content: [
        { type: 'heading', attrs: { level: 2 }, content: [t('Launch plan')] },
        p(
            t('Bold', [{ type: 'bold' }]), t(' '),
            t('italic', [{ type: 'italic' }]), t(' '),
            t('struck', [{ type: 'strike' }]), t(' '),
            t('code', [{ type: 'code' }]), t(' '),
            t('under', [{ type: 'underline' }]), t(' '),
            t('red', [{ type: 'textStyle', attrs: { color: '#ff0000' } }]), t(' '),
            t('a link', [{ type: 'link', attrs: { href: 'https://onemana.dev/x', target: '_blank', rel: 'noopener noreferrer nofollow', class: 'link' } }]),
            { type: 'hardBreak' },
            t('Ask '),
            { type: 'mention', attrs: { id: 'u-1', label: 'Sam Rivera', mentionSuggestionChar: '@' } },
            t(' in '),
            { type: 'channelMention', attrs: { id: 'c-1', label: 'engineering', mentionSuggestionChar: '#' } },
            t(' about '),
            { type: 'entityMention', attrs: { id: 'doc:d-1', label: 'Launch notes', mentionSuggestionChar: '+' } },
        ),
        { type: 'blockquote', content: [p(t('Quoted.'))] },
        { type: 'bulletList', content: [{ type: 'listItem', content: [p(t('One'))] }] },
        { type: 'orderedList', attrs: { start: 3, type: 'a' }, content: [{ type: 'listItem', content: [p(t('Three'))] }] },
        { type: 'taskList', content: [{ type: 'taskItem', attrs: { checked: true }, content: [p(t('Done'))] }] },
        { type: 'horizontalRule' },
        { type: 'codeBlock', attrs: { language: 'go' }, content: [t('fmt.Println("hi")')] },
        {
            type: 'image',
            attrs: { src: 'https://onemana.dev/a.png', alt: 'A chart', title: 'Chart', id: 'img-1', width: 300, height: 200, caption: 'Revenue by month', fileName: 'chart.png' },
        },
        { type: 'callout', attrs: { emoji: '🚀', color: 'green' }, content: [p(t('Ship it.'))] },
        { type: 'collapsible', attrs: { title: 'Details', open: false }, content: [p(t('Hidden.'))] },
        { type: 'tableEmbed', attrs: { tableId: 'tbl-1', viewId: 'view-1' } },
        { type: 'poll', attrs: { id: 'poll-1' } },
        { type: 'chartEmbed', attrs: { spec: '{"type":"bar","data":[1,2]}' } },
        { type: 'diffEmbed', attrs: { diff: '--- a\n+++ b\n@@ -1 +1 @@\n-x\n+y' } },
        { type: 'recordingEmbed', attrs: { egress: 'eg-1', kind: 'audio', sid: 'room-1', u1: 'https://x/1', u2: 'https://x/2' } },
    ],
}

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
