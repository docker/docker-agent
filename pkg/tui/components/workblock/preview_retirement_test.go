package workblock

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestReasoningPreviewPersistsAfterReasoningAndWorkSettle(t *testing.T) {
	for _, count := range []int{0, 1, 2, 3, 5} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			clock := &fakeClock{now: time.Unix(1, 0)}
			ar := animation.NewRuntimeWithScheduler(clock)
			t.Cleanup(ar.Stop)
			m := New(ar)
			t.Cleanup(m.StopAnimation)
			child := reasoningblock.New(ar, "r", "root", service.StaticSessionState{})
			lines := make([]string, count)
			for i := range lines {
				lines[i] = fmt.Sprintf("thought-%d", i)
			}
			child.SetReasoning(strings.Join(lines, "\n\n"))
			m.SetItems([]Item{{SourceIndex: 1, View: child}})
			require.Equal(t, 1+min(count, 3), m.Height(), "replayed reasoning uses only actual rows")
			child.SetReasoningActive(true)
			m.SetStatus(Status{Active: true, Waiting: "Waiting"})
			m.InvalidateSource(1)
			if count > 0 {
				require.Equal(t, 4, m.Height(), "live reasoning keeps its existing budget")
			}
			child.SetReasoningActive(false)
			m.InvalidateSource(1)
			require.Equal(t, 1+min(count, 3), m.Height(), "prose boundary retains text without blank rows")
			require.True(t, m.Status().Active)
			m.SetStatus(Status{})
			advance(t, ar, m, clock, time.Second)
			require.Equal(t, 1+min(count, 3), m.Height())
			require.False(t, m.NeedsTick())
			require.Zero(t, ar.ActiveCount())
			require.Empty(t, m.Rows(), "preview is inert")
			for _, line := range lines[max(0, count-3):] {
				require.Contains(t, ansi.Strip(m.View()), line)
			}
			m.SetExpandedImmediately(true)
			for _, line := range lines {
				require.Equal(t, 1, strings.Count(ansi.Strip(m.View()), line), "expanded history never duplicates the preview")
			}
			m.SetExpandedImmediately(false)
			require.Equal(t, 1+min(count, 3), m.Height())
		})
	}
}

func TestRetainedReasoningSurvivesResizeExpandAndHide(t *testing.T) {
	for _, change := range []string{"resize", "expand", "hide"} {
		t.Run(change, func(t *testing.T) {
			ar := animation.NewRuntime()
			t.Cleanup(ar.Stop)
			m := New(ar)
			t.Cleanup(m.StopAnimation)
			child := reasoningblock.New(ar, "r", "root", service.StaticSessionState{})
			child.SetReasoning("displayable history")
			m.SetItems([]Item{{SourceIndex: 1, View: child}})
			switch change {
			case "resize":
				m.SetSize(40, 20)
			case "expand":
				m.SetExpandedImmediately(true)
			case "hide":
				m.StopAnimation()
				m.ResumeAnimation()
			}
			require.False(t, m.previewRetire.Running())
			require.Zero(t, ar.ActiveCount())
			m.SetExpandedImmediately(false)
			require.Equal(t, 2, m.Height())
			require.Contains(t, ansi.Strip(m.View()), "displayable history")
		})
	}
}
