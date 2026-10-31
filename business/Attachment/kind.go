package business

import (
	"path/filepath"
	"strings"
)

// KindOf is what kind of file a name is, as the app shows attachments:
// "image", "document", "audio", "video" or "other". The web's twin is
// lib/utils/file/getAttachmentType.ts. Pure.
func KindOf(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp":
		return "image"
	case ".pdf", ".txt", ".doc", ".docx", ".xls", ".xlsx", ".csv":
		return "document"
	case ".mp3", ".wav", ".ogg", ".m4a", ".weba":
		return "audio"
	case ".mp4", ".mov", ".webm", ".avi", ".mkv":
		return "video"
	default:
		return "other"
	}
}
