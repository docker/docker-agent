//go:build js

package datetime

import (
	"syscall/js"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetDatetimeBrowserLocalTimezone(t *testing.T) {
	zone := "America/New_York"
	format := js.FuncOf(func(js.Value, []js.Value) any {
		return map[string]any{"timeZone": zone}
	})
	t.Cleanup(format.Release)
	dateTimeFormat := js.FuncOf(func(js.Value, []js.Value) any {
		return map[string]any{"resolvedOptions": format}
	})
	t.Cleanup(dateTimeFormat.Release)
	setBrowserIntl(t, js.ValueOf(map[string]any{"DateTimeFormat": dateTimeFormat}))

	now := time.Date(2026, time.October, 7, 23, 4, 5, 0, time.UTC)
	ts := &ToolSet{now: func() time.Time { return now }}
	checkLocal := func(want string) {
		t.Helper()
		for _, timezone := range []string{"", "Local"} {
			result, err := ts.get(t.Context(), Args{Format: time.RFC3339, Timezone: timezone})
			require.NoError(t, err)
			require.False(t, result.IsError)
			assert.Equal(t, want, result.Output)
		}
	}
	checkLocal("2026-10-07T19:04:05-04:00")
	now = now.AddDate(0, 1, 0)
	checkLocal("2026-11-07T18:04:05-05:00")
	zone = "Asia/Tokyo"
	checkLocal("2026-11-08T08:04:05+09:00")
}

func TestGetDatetimeBrowserLocalFallback(t *testing.T) {
	prototype := js.Global().Get("Date").Get("prototype")
	original := prototype.Get("getTimezoneOffset")
	offset := js.FuncOf(func(this js.Value, _ []js.Value) any {
		if this.Call("getUTCMonth").Int() == 9 {
			return 240
		}
		return 300
	})
	prototype.Set("getTimezoneOffset", offset)
	t.Cleanup(func() {
		prototype.Set("getTimezoneOffset", original)
		offset.Release()
	})

	throwing := js.Global().Get("Function").New("throw new Error('Intl unavailable')")
	for name, intl := range map[string]js.Value{
		"missing Intl":       js.Undefined(),
		"missing formatter":  js.ValueOf(map[string]any{}),
		"unknown zone":       browserIntlForZone(t, "Unknown/Zone"),
		"empty zone":         browserIntlForZone(t, ""),
		"local zone":         browserIntlForZone(t, "Local"),
		"throwing formatter": js.ValueOf(map[string]any{"DateTimeFormat": throwing}),
	} {
		t.Run(name, func(t *testing.T) {
			setBrowserIntl(t, intl)
			now := time.Date(2026, time.October, 7, 23, 4, 5, 0, time.UTC)
			ts := &ToolSet{now: func() time.Time { return now }}
			for _, want := range []string{"2026-10-07T19:04:05-04:00", "2026-11-07T18:04:05-05:00"} {
				for _, timezone := range []string{"", "Local"} {
					result, err := ts.get(t.Context(), Args{Format: time.RFC3339, Timezone: timezone})
					require.NoError(t, err)
					require.False(t, result.IsError)
					assert.Equal(t, want, result.Output)
				}
				now = now.AddDate(0, 1, 0)
			}
		})
	}
}

func setBrowserIntl(t *testing.T, intl js.Value) {
	t.Helper()
	original := js.Global().Get("Intl")
	js.Global().Set("Intl", intl)
	t.Cleanup(func() { js.Global().Set("Intl", original) })
}

func browserIntlForZone(t *testing.T, zone string) js.Value {
	t.Helper()
	resolvedOptions := js.FuncOf(func(js.Value, []js.Value) any {
		return map[string]any{"timeZone": zone}
	})
	t.Cleanup(resolvedOptions.Release)
	dateTimeFormat := js.FuncOf(func(js.Value, []js.Value) any {
		return map[string]any{"resolvedOptions": resolvedOptions}
	})
	t.Cleanup(dateTimeFormat.Release)
	return js.ValueOf(map[string]any{"DateTimeFormat": dateTimeFormat})
}
