//go:build !js

package datetime

import "time"

func localTime(now time.Time) time.Time {
	return now.In(time.Local)
}
