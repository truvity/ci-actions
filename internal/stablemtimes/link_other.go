//go:build !linux

package stablemtimes

import (
	"errors"
	"time"
)

var errUnsupported = errors.New("setting a symlink's own time is not supported here")

func setLinkTime(string, time.Time) error { return errUnsupported }

func isUnsupported(err error) bool { return errors.Is(err, errUnsupported) }
