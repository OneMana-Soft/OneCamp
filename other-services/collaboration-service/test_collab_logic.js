
import { TiptapTransformer } from '@hocuspocus/transformer'
import { generateJSON } from '@tiptap/html'
import { StarterKit } from '@tiptap/starter-kit'
import { Image } from '@tiptap/extension-image'
import { Link } from '@tiptap/extension-link'
import { TextStyle } from '@tiptap/extension-text-style'
import { Color } from '@tiptap/extension-color'
import { Mention } from '@tiptap/extension-mention'
import { Typography } from '@tiptap/extension-typography'
import { CodeBlockLowlight } from '@tiptap/extension-code-block-lowlight'
import { common, createLowlight } from 'lowlight'

const lowlight = createLowlight(common)

const docBody = '<p xmlns="http://www.w3.org/1999/xhtml">hjh</p>'
console.log('Testing with docBody:', docBody)

try {
    const json = generateJSON(docBody, [
        StarterKit.configure({ codeBlock: false }),
        Image,
        TextStyle,
        Color,
        Mention,
        Typography,
        CodeBlockLowlight.configure({ lowlight }),
    ])

    console.log('Generated JSON:', JSON.stringify(json, null, 2))

    const ydoc = TiptapTransformer.toYdoc(json, 'default', [
        StarterKit.configure({ codeBlock: false }),
        Image,
        TextStyle,
        Color,
        Mention,
        Typography,
        CodeBlockLowlight.configure({ lowlight }),
    ])

    console.log('Yjs Doc created successfully')
    // Check if content exists in Ydoc
    const ytext = ydoc.getXmlFragment('default')
    console.log('YDoc XML Fragment content:', ytext.toJSON())

} catch (error) {
    console.error('Error during transformation:', error)
}
