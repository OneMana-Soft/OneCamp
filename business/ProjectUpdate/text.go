package business

// An update's text is plain text (helpers.PlainTextToHTML): paragraphs split by
// a blank line, and lines that start with "- ", "* " or "• " are a list. It
// reads the same in an email, a notification, the client's page and a
// terminal, needs no editor to write, and can't carry markup anyone could
// abuse. The app renders it with the same rules (lib/projectUpdates.ts).

// MaxBody is the longest an update may be, in characters.
const MaxBody = 8000
