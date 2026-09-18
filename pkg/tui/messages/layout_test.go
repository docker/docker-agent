package messages

import "testing"

func TestLayoutSettingsDefaults(t *testing.T) {
	t.Parallel()

	layout := LayoutSettings{}
	if layout.ShowPlans {
		t.Error("the Plans section must be hidden by default")
	}
	if layout.HideSessionPath || layout.HideUsage || layout.HideAgents || layout.HideTools || layout.HideTodos {
		t.Error("existing sidebar sections must remain visible by default")
	}
}

func TestParseSectionSpacing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw  string
		want SectionSpacing
	}{
		{"", SpacingNormal},
		{"normal", SpacingNormal},
		{"compact", SpacingCompact},
		{"relaxed", SpacingRelaxed},
		{"bogus", SpacingNormal},
	}
	for _, tt := range tests {
		if got := ParseSectionSpacing(tt.raw); got != tt.want {
			t.Errorf("ParseSectionSpacing(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}

func TestParseSidebarInfoMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw  string
		want SidebarInfoMode
	}{
		{"", InfoModeCompact},
		{"compact", InfoModeCompact},
		{"detailed", InfoModeDetailed},
		{"bogus", InfoModeCompact},
	}
	for _, tt := range tests {
		if got := ParseSidebarInfoMode(tt.raw); got != tt.want {
			t.Errorf("ParseSidebarInfoMode(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}

func TestSectionSpacingBlankLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		spacing SectionSpacing
		want    int
	}{
		{SpacingCompact, 1},
		{SpacingNormal, 2},
		{SpacingRelaxed, 3},
		{SectionSpacing(""), 2},
		{SectionSpacing("bogus"), 2},
	}
	for _, tt := range tests {
		if got := tt.spacing.BlankLines(); got != tt.want {
			t.Errorf("SectionSpacing(%q).BlankLines() = %d, want %d", tt.spacing, got, tt.want)
		}
	}
}
