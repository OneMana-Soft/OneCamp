// The editor's schema, shared by the server and anything that converts HTML
// to and from the collaborative document: one file per block, in the order the
// web editor lists them. It MUST match the web editor exactly: a node the
// server does not know made every save of a doc holding it fail ("Unknown node
// type: tableEmbed"), and an attribute it does not know is dropped from the
// saved body (an image's caption was).
//
// The web's block registry generates schema-manifest.json from its specs;
// schemaParity.test.js holds this list to exactly that file, and
// scripts/schemaManifest.js takes a new one from the web (docs programme I4).
//
// A package adds its block's file here, one line, when it merges
// (PROGRAM.md §3.6).
import { StarterKitSchema } from './starterKit.js'
import { LinkSchema } from './link.js'
import { UnderlineSchema } from './underline.js'
import { ImageSchema } from './image.js'
import { ColorSchema, TextStyleSchema } from './textStyle.js'
import { MentionSchema, ChannelMentionSchema, EntityMentionSchema } from './mentions.js'
import { TypographySchema } from './typography.js'
import { TaskItemSchema, TaskListSchema } from './taskList.js'
import { CodeBlockSchema } from './codeBlock.js'
import { CalloutSchema } from './callout.js'
import { CollapsibleSchema } from './collapsible.js'
import { ChartEmbedSchema, DiffEmbedSchema, PollSchema, RecordingEmbedSchema, TableEmbedSchema } from './embeds.js'
import { BlockIdSchema } from './blockId.js'
import { IndentSchema } from './indent.js'
import { BlockDirSchema } from './blockDir.js'
import { ColumnListSchema, ColumnSchema } from './columns.js'
import { HighlightSchema } from './highlight.js' // B2
import { BlockColorSchema } from './blockColor.js' // B2
import { QuoteSizeSchema } from './quoteSize.js' // B2
import { TocSchema } from './toc.js' // B2
import { TableSchema, TableRowSchema, TableHeaderSchema, TableCellSchema } from './simpleTable.js'
import { InlineMathSchema, BlockMathSchema } from './math.js'
import { BookmarkSchema, FileBlockSchema, LinkMentionSchema, WebEmbedSchema } from './media.js'
import { BoardEmbedSchema, DateMentionSchema, SubpageSchema, TaskItemLinkSchema, TaskListEmbedSchema } from './connected.js' // C1

export const COLLAB_EXTENSIONS = [
    StarterKitSchema,
    LinkSchema,
    UnderlineSchema,
    ImageSchema,
    TextStyleSchema,
    ColorSchema,
    MentionSchema,
    TypographySchema,
    TaskListSchema,
    TaskItemSchema,
    CodeBlockSchema,
    CalloutSchema,
    CollapsibleSchema,
    TableEmbedSchema,
    PollSchema,
    ChartEmbedSchema,
    DiffEmbedSchema,
    RecordingEmbedSchema,
    ChannelMentionSchema,
    EntityMentionSchema,
    BlockIdSchema,
    // Blocks added by the docs programme's packages, one line each:
    IndentSchema, BlockDirSchema, ColumnListSchema, ColumnSchema, // B1: indent, direction, columns
    HighlightSchema, BlockColorSchema, QuoteSizeSchema, TocSchema, // B2: highlight, block colour, large quote, contents
    TableSchema, TableRowSchema, TableHeaderSchema, TableCellSchema, InlineMathSchema, BlockMathSchema, // B3: simple tables, equations
    BookmarkSchema, WebEmbedSchema, FileBlockSchema, LinkMentionSchema, // B4: links and media
    SubpageSchema, DateMentionSchema, TaskListEmbedSchema, BoardEmbedSchema, TaskItemLinkSchema, // C1: pages, dates, task lists, boards, to-dos that are tasks
]
