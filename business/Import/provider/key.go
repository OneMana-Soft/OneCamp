package provider

import (
	"strconv"
	"strings"
)

// SplitKey is a source key's project prefix and number ("ENG-123" is ENG and
// 123, as task keys are written: models/postgres/TaskKey), upper-cased; "" and
// 0 for anything else.
func SplitKey(raw string) (string, int) {
	i := strings.LastIndexByte(raw, '-')
	if i <= 0 || i == len(raw)-1 {
		return "", 0
	}
	n, err := strconv.Atoi(raw[i+1:])
	if err != nil || n < 1 {
		return "", 0
	}
	return strings.ToUpper(raw[:i]), n
}
