package router

import (
	notificationController "github.com/akashc777/OneCamp/controllers/Notification"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
)

// mountChatNotify is "Why did or didn't I get this?" (A4 H3, the
// Notification Router's own path): Settings > Notifications' "Send me a
// test", through the router to the session's own devices, one a minute; and
// each Inbox item's "Why am I seeing this?", the session's own record of its
// message. Member is a signed-in session's alone. The demo's shared visitor
// sends no test: its devices are every visitor's.
//
// Permission model: any member, guests included, for themselves alone;
// nothing about anyone else is read or shown.
func mountChatNotify(rt chatRouters) {
	rt.Member.With(customMiddleware.NoDeviceTestsInDemo).Post("/notify/test", notificationController.SendMeATest)
	rt.Member.Get("/notify/why/{message_id}", notificationController.WhyAmISeeingThis)
}
