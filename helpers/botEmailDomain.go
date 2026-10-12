package helpers

// BotEmailDomain is the reserved domain every bot principal's email sits
// under (domain/User names it too). Not a real mail domain. It lives here so
// that checks below the domain layer, such as who may notify through a pause
// (business/Notification), read the same value.
const BotEmailDomain = "@bot.onecamp.local"
