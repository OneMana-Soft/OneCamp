// The link mark, as the web editor's (extensions/link/link.ts).
import { Link } from '@tiptap/extension-link'

export const LinkSchema = Link.configure({
    openOnClick: false,
    autolink: true,
    defaultProtocol: 'https',
})
