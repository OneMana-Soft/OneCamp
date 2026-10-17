// The doc colours by name (docs programme B2, N15), as the web stores them
// (web lib/docColours.ts and extensions/color/color.ts): text "sky", a block's
// background "sky_background". The same names everywhere a doc goes, so it
// reads in both themes and outside the app.

export const DOC_COLOURS = ['gray', 'sun', 'moss', 'lake', 'sky', 'dusk', 'berry']

export const isDocColour = (v) => DOC_COLOURS.includes(v)

// The blocks that take a colour of their own (blockColor).
export const BLOCK_COLOUR_NODES = ['paragraph', 'heading', 'blockquote', 'listItem', 'taskItem', 'collapsible']

const NAMES = `(${DOC_COLOURS.join('|')})`
const STORED = new RegExp(`^(?:${NAMES}|var\\(--camp-${NAMES}-ink\\))$`)

// colourName is a text colour as stored: its name. An old doc's
// var(--camp-sky-ink) is "sky"; any other colour (a paste's black) is none.
export function colourName(v) {
    const m = STORED.exec(String(v ?? '').trim().toLowerCase())
    return m ? (m[1] ?? m[2]) : null
}

// blockColourValue is a block's stored colour ("sky", "sky_background"), or
// null when it is not one of ours.
export function blockColourValue(v) {
    if (typeof v !== 'string') return null
    const [colour, background, ...rest] = v.split('_')
    return !rest.length && isDocColour(colour) && (background === undefined || background === 'background') ? v : null
}

// styleColor is the color an element's style attribute names, read from the
// attribute itself (the server's DOM computes no styles).
export function styleColor(el) {
    const style = el.getAttribute?.('style') || ''
    const decls = style.split(';').map((s) => s.trim()).filter(Boolean)
    for (let i = decls.length - 1; i >= 0; i--) {
        const at = decls[i].indexOf(':')
        if (at > 0 && decls[i].slice(0, at).trim().toLowerCase() === 'color') return decls[i].slice(at + 1).trim().replace(/['"]+/g, '')
    }
    return null
}
