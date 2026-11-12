//go:build js

package datetime

import (
	"syscall/js"
	"time"
)

func localTime(now time.Time) time.Time {
	// Go's WASM time.Local caches a fixed offset, without DST transitions.
	if location := browserLocation(); location != nil {
		return now.In(location)
	}
	offset := js.Global().Get("Date").New(now.UnixMilli()).Call("getTimezoneOffset").Int()
	return now.In(time.FixedZone("Local", -offset*60))
}

func browserLocation() (location *time.Location) {
	// Intl may be missing or throw in restricted JavaScript hosts.
	defer func() {
		if recover() != nil {
			location = nil
		}
	}()
	intl := js.Global().Get("Intl")
	if intl.Type() != js.TypeObject || intl.Get("DateTimeFormat").Type() != js.TypeFunction {
		return nil
	}
	zone := intl.Call("DateTimeFormat").Call("resolvedOptions").Get("timeZone")
	if zone.Type() != js.TypeString || zone.String() == "" || zone.String() == "Local" {
		return nil
	}
	location, _ = time.LoadLocation(zone.String())
	return location
}
