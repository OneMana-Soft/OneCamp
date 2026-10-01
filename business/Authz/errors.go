package business

import "errors"

var (
	errInvalidCapability = errors.New("unknown capability")
	errInvalidPolicy     = errors.New("invalid policy (must be admins_only or all_members)")
)
