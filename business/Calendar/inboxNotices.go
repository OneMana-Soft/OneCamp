package Calendar

// The inbox names a calendar notice (invited, changed, cancelled, answered,
// a reminder) and opens it only while its reader may open the event now, as
// it does a doc's and a table row's (business/Follow noticeKinds.go): the
// Access Resolver's titles say which, and a reader it can't answer for is
// told nothing more than an untitled notice (fail closed).

import followBusiness "github.com/akashc777/OneCamp/business/Follow"

func init() { followBusiness.RegisterNoticeKind(KindEvent, followBusiness.TitledByAccess(KindEvent)) }
