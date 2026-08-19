package hubauth

import (
	"net/http"
	"sync/atomic"
	"time"
)

// skewThreshold is how far our clock must differ from Docker's before we
// correct for it: smaller differences are dominated by request latency and the
// one-second resolution of the Date header.
const skewThreshold = 5 * time.Second

// clockSkew is how far this machine's clock is behind Docker's, in
// nanoseconds. A machine resuming from sleep, or a VM with a drifting clock,
// can be minutes off — enough to make every fresh token look expired (or a
// dead one look valid) and to defeat every expiry decision below.
var clockSkew atomic.Int64

// now returns the current time as Docker sees it.
func now() time.Time {
	return time.Now().Add(time.Duration(clockSkew.Load()))
}

// learnClockSkew records how far our clock is from the one of the server that
// issues our tokens.
func learnClockSkew(header http.Header) {
	date, err := http.ParseTime(header.Get("Date"))
	if err != nil {
		return
	}

	skew := time.Until(date)
	if skew > -skewThreshold && skew < skewThreshold {
		clockSkew.Store(0)
		return
	}
	clockSkew.Store(int64(skew))
}
