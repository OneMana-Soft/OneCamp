package helpers

import (
	"strconv"
	"strings"
)

// ReadableBytes is a size in the units an upload limit is set in (1 GB =
// 1024 MB), with one decimal only when it isn't whole: "5 GB", "7.2 GB",
// "640 MB". The web app writes sizes the same way, so a limit the server
// states and the one the upload dialog shows read alike.
func ReadableBytes(b int64) string {
	if b < 0 {
		b = 0
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	v, i := float64(b), 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return strings.TrimSuffix(strconv.FormatFloat(v, 'f', 1, 64), ".0") + " " + units[i]
}
