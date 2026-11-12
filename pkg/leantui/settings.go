package leantui

import (
	"fmt"

	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/userconfig"
)

const (
	settingSendMode = iota
	settingYOLO
	settingSnapshot
	settingCacheStablePrompts
	settingBackgroundCompaction
	settingBackgroundCompactionThreshold
	settingWarnOnCacheMiss
	settingLean
	settingKeepAwake
	settingSound
	settingSoundThreshold
	settingRenderImages
	settingShowBanner
	settingSplitDiff
	settingFullFilePreview
	settingCount
)

type leanSettings struct {
	values                      [settingCount]bool
	original                    [settingCount]bool
	soundThreshold              int
	originalThreshold           int
	backgroundThreshold         int
	originalBackgroundThreshold int
}

func (m *model) openSettings() {
	s := userconfig.Get()
	values := [settingCount]bool{
		settingFullFilePreview:      m.screen.Transcript.FullFilePreview(),
		settingSendMode:             m.sendMode == messages.SendModeQueue,
		settingYOLO:                 s.YOLO,
		settingSnapshot:             s.SnapshotsEnabled(),
		settingCacheStablePrompts:   s.CacheStablePromptsEnabled(),
		settingBackgroundCompaction: s.BackgroundCompaction,
		settingWarnOnCacheMiss:      s.CacheMissWarningsEnabled(),
		settingLean:                 s.Lean,
		settingKeepAwake:            s.GetKeepAwake(),
		settingSound:                s.GetSound(),
		settingRenderImages:         s.GetRenderImages(),
		settingShowBanner:           !m.hideBanner,
		settingSplitDiff:            m.sessionState.SplitDiffView(),
	}
	m.settings = &leanSettings{
		values: values, original: values,
		soundThreshold: s.GetSoundThreshold(), originalThreshold: s.GetSoundThreshold(),
		backgroundThreshold: s.GetBackgroundCompactionThreshold(), originalBackgroundThreshold: s.GetBackgroundCompactionThreshold(),
	}
	m.screen.Settings = &ui.SettingsModel{}
	m.screen.Autocomplete.Dismiss()
	m.refreshSettingsRows()
}

func (m *model) refreshSettingsRows() {
	labels := [settingCount]string{
		"While agent is working",
		"Auto-approve tools by default (next launch)",
		"Automatic snapshots (next launch)",
		"Cache-stable dynamic prompts",
		"Background compaction (next conversation turn)",
		"Background compaction threshold",
		"Warn on cache miss",
		"Lean UI by default (next launch)",
		"Keep machine awake (next agent run)",
		"Completion sound",
		"Sound threshold",
		"Render images (next launch if not detected)",
		"Show startup banner (next launch)",
		"Split diff view",
		"Written file preview",
	}
	rows := make([]ui.SettingRow, settingCount)
	for i, label := range labels {
		value := "Off"
		if m.settings.values[i] {
			value = "On"
		}
		if i == settingSendMode {
			value = "Steer"
			if m.settings.values[i] {
				value = "Queue"
			}
		}
		if i == settingFullFilePreview {
			value = "Last 10 lines"
			if m.settings.values[i] {
				value = "Full"
			}
		}
		if i == settingSoundThreshold {
			value = fmt.Sprintf("%d seconds", m.settings.soundThreshold)
			if !m.settings.values[settingSound] {
				value += " (sound off)"
			}
		}
		if i == settingBackgroundCompactionThreshold {
			value = fmt.Sprintf("%d%%", m.settings.backgroundThreshold)
		}
		rows[i] = ui.SettingRow{Label: label, Value: value}
	}
	m.screen.Settings.Rows = rows
}

func (m *model) handleSettingsKey(k ui.Key) {
	panel := m.screen.Settings
	switch k.Typ {
	case ui.KeyEsc, ui.KeyCtrlC:
		m.closeSettings()
		return
	case ui.KeyUp:
		panel.Selected = max(0, panel.Selected-1)
		panel.ConfirmYOLO = false
	case ui.KeyDown:
		panel.Selected = min(settingCount-1, panel.Selected+1)
		panel.ConfirmYOLO = false
	case ui.KeyHome:
		panel.Selected = 0
		panel.ConfirmYOLO = false
	case ui.KeyEnd:
		panel.Selected = settingCount - 1
		panel.ConfirmYOLO = false
	case ui.KeyLeft:
		m.changeSetting(-1)
	case ui.KeyRight:
		m.changeSetting(1)
	case ui.KeyRune:
		if string(k.Runes) == " " {
			m.changeSetting(1)
		}
	case ui.KeyEnter:
		if m.settings.values != m.settings.original || m.settings.soundThreshold != m.settings.originalThreshold || m.settings.backgroundThreshold != m.settings.originalBackgroundThreshold {
			if err := saveLeanSettings(m.settings); err != nil {
				m.addNotice("✗ ", "Failed to save settings: "+err.Error(), ui.StError())
				return
			}
			m.sendMode = messages.SendModeSteer
			if m.settings.values[settingSendMode] {
				m.sendMode = messages.SendModeQueue
			}
			if m.settings.values[settingRenderImages] != m.settings.original[settingRenderImages] {
				m.renderImages = m.settings.values[settingRenderImages] && m.imageSupport
			}
			m.hideBanner = !m.settings.values[settingShowBanner]
			m.sessionState.SetSplitDiffView(m.settings.values[settingSplitDiff])
			m.screen.Transcript.SetFullFilePreview(m.settings.values[settingFullFilePreview])
			m.addNotice("", "Settings updated.", ui.StMuted())
		}
		m.closeSettings()
		return
	}
	m.refreshSettingsRows()
}

func (m *model) changeSetting(delta int) {
	panel := m.screen.Settings
	row := panel.Selected
	if row == settingBackgroundCompactionThreshold {
		m.settings.backgroundThreshold = max(1, min(99, m.settings.backgroundThreshold+delta))
		return
	}
	if row == settingSoundThreshold {
		if m.settings.values[settingSound] {
			m.settings.soundThreshold = max(1, min(300, m.settings.soundThreshold+delta))
		}
		return
	}
	if row == settingYOLO && !m.settings.values[row] && !panel.ConfirmYOLO {
		panel.ConfirmYOLO = true
		return
	}
	m.settings.values[row] = !m.settings.values[row]
	panel.ConfirmYOLO = false
}

func (m *model) closeSettings() {
	m.settings = nil
	m.screen.Settings = nil
}

func saveLeanSettings(p *leanSettings) error {
	return userconfig.Update(func(cfg *userconfig.Config) error {
		if cfg.Settings == nil {
			cfg.Settings = &userconfig.Settings{}
		}
		s := cfg.Settings
		for i, value := range p.values {
			if value == p.original[i] {
				continue
			}
			switch i {
			case settingSendMode:
				s.BusySendMode = ""
				if value {
					s.BusySendMode = string(messages.SendModeQueue)
				}
			case settingYOLO:
				s.YOLO = value
			case settingSnapshot:
				s.Snapshot = enabledPreference(value)
			case settingBackgroundCompaction:
				s.BackgroundCompaction = value
			case settingCacheStablePrompts:
				s.CacheStablePrompts = enabledPreference(value)
			case settingWarnOnCacheMiss:
				s.WarnOnCacheMiss = enabledPreference(value)
			case settingLean:
				s.Lean = value
			case settingKeepAwake:
				s.KeepAwake = value
			case settingSound:
				s.Sound = value
			case settingRenderImages:
				s.RenderImages = defaultOnPreference(value)
			case settingShowBanner:
				s.ShowBanner = defaultOnPreference(value)
			case settingFullFilePreview:
				s.LeanFullFilePreview = value
			case settingSplitDiff:
				s.SplitDiffView = defaultOnPreference(value)
			}
		}
		if p.backgroundThreshold != p.originalBackgroundThreshold {
			s.BackgroundCompactionThreshold = p.backgroundThreshold
			if s.BackgroundCompactionThreshold == userconfig.DefaultBackgroundCompactionThreshold {
				s.BackgroundCompactionThreshold = 0
			}
		}
		if p.soundThreshold != p.originalThreshold {
			s.SoundThreshold = p.soundThreshold
			if s.SoundThreshold == userconfig.DefaultSoundThreshold {
				s.SoundThreshold = 0
			}
		}
		return nil
	})
}

func enabledPreference(enabled bool) *bool {
	if !enabled {
		return nil
	}
	return new(true)
}

func defaultOnPreference(enabled bool) *bool {
	if enabled {
		return nil
	}
	return new(false)
}
