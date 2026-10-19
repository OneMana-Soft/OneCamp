package router

// Comments on a table's rows (tables A8) ride the threads routes
// (docsThreads.go): business/TableComments registers the table_row kind for
// them, with its live messages, notices and inbox naming, from its init. This
// file is what links it in.
import _ "github.com/akashc777/OneCamp/business/TableComments"
