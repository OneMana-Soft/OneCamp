package business

// The inbox names a project's notice (to its admins, say) and opens it only
// while its reader may open the project now, as it does a doc's, a table
// row's and an event's (business/Follow noticeKinds.go): the Access
// Resolver's titles say which, and a reader it can't answer for is told
// nothing more than an untitled notice (fail closed). Registered here, with
// projects, so any package's project notice is named the same way.

import (
	followBusiness "github.com/akashc777/OneCamp/business/Follow"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
)

func init() {
	followBusiness.RegisterNoticeKind(workAccess.KindProject, followBusiness.TitledByAccess(workAccess.KindProject))
}
