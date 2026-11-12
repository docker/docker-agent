package tui

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/statusbar"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type themeWatchRun struct {
	ctx  context.Context //nolint:containedctx // Captures the run context for cancellation assertions.
	ref  string
	done chan struct{}
}

type fakeThemeWatcher struct {
	runs          chan themeWatchRun
	attach        <-chan struct{}
	release       <-chan struct{}
	notify        func(context.Context, string)
	startupResult func() error
}

func (f *fakeThemeWatcher) Run(ctx context.Context, themeRef string, ready func()) error {
	done := make(chan struct{})
	defer close(done)
	f.runs <- themeWatchRun{ctx: ctx, ref: themeRef, done: done}
	if f.startupResult != nil {
		return f.startupResult()
	}
	if f.attach != nil {
		select {
		case <-f.attach:
		case <-ctx.Done():
			return nil
		}
	}
	ready()
	if f.notify != nil {
		f.notify(ctx, themeRef)
	}
	<-ctx.Done()
	if f.release != nil {
		<-f.release
	}
	return nil
}

func setupThemeWatcher(t *testing.T, m *appModel) *fakeThemeWatcher {
	t.Helper()
	m.themeCtx, m.cancelThemes = context.WithCancel(t.Context())
	t.Cleanup(m.cleanupManagedResources)
	fw := &fakeThemeWatcher{runs: make(chan themeWatchRun, 10)}
	m.themeWatcher = fw
	return fw
}

func nextThemeWatchRun(t *testing.T, fw *fakeThemeWatcher) themeWatchRun {
	t.Helper()
	select {
	case run := <-fw.runs:
		return run
	case <-time.After(5 * time.Second):
		t.Fatal("theme watcher did not run")
		return themeWatchRun{}
	}
}

func waitThemeWatchRun(t *testing.T, run themeWatchRun) {
	t.Helper()
	select {
	case <-run.done:
	case <-time.After(5 * time.Second):
		t.Fatal("theme watcher did not stop")
	}
	assert.ErrorIs(t, run.ctx.Err(), context.Canceled)
}

// switchTheme applies ref until the test ends; the theme registry is process-global.
func switchTheme(t *testing.T, ref string) {
	t.Helper()
	previous := styles.CurrentTheme().Ref
	t.Cleanup(func() { styles.ApplyThemeRef(previous) })
	styles.ApplyThemeRef(ref)
}

func TestWatchCurrentTheme_TargetsAppliedTheme(t *testing.T) {
	m, _ := newTestModel(t)
	fw := setupThemeWatcher(t, m)

	m.watchCurrentTheme()

	assert.Equal(t, styles.CurrentTheme().Ref, nextThemeWatchRun(t, fw).ref)
}

func TestWatchCurrentTheme_NoWatcherIsNoop(t *testing.T) {
	m, _ := newTestModel(t)

	assert.NotPanics(t, func() { m.watchCurrentTheme() })
}

func TestApplyThemeChanged_RetargetsWatcher(t *testing.T) {
	m, _ := newTestModel(t)
	m.statusBar = statusbar.New(m)
	fw := setupThemeWatcher(t, m)
	m.watchCurrentTheme()
	first := nextThemeWatchRun(t, fw)

	switchTheme(t, "dracula")
	_, _ = m.applyThemeChanged()

	second := nextThemeWatchRun(t, fw)
	assert.Equal(t, "dracula", second.ref)
	waitThemeWatchRun(t, first)
	require.NoError(t, second.ctx.Err())
}

// Hot reloads re-apply the same theme; a restart would drop an edit still
// being debounced.
func TestApplyThemeChanged_KeepsRunOnSameTheme(t *testing.T) {
	m, _ := newTestModel(t)
	m.statusBar = statusbar.New(m)
	fw := setupThemeWatcher(t, m)
	m.watchCurrentTheme()
	run := nextThemeWatchRun(t, fw)

	_, _ = m.applyThemeChanged()

	assert.Empty(t, fw.runs)
	require.NoError(t, run.ctx.Err())
}

func TestCleanupManagedResources_CancelsThemeWatcher(t *testing.T) {
	m, _ := newTestModel(t)
	fw := setupThemeWatcher(t, m)
	m.watchCurrentTheme()
	run := nextThemeWatchRun(t, fw)

	m.cleanupManagedResources()

	waitThemeWatchRun(t, run)

	// A late theme change must not outlive shutdown either.
	m.watchCurrentTheme()
	assert.Empty(t, fw.runs)
}

func TestWatchCurrentTheme_ConcurrentShutdown(t *testing.T) {
	m, _ := newTestModel(t)
	fw := setupThemeWatcher(t, m)
	var shutdown sync.WaitGroup
	shutdown.Go(m.cleanupManagedResources)

	m.watchCurrentTheme()
	shutdown.Wait()

	select {
	case run := <-fw.runs:
		waitThemeWatchRun(t, run)
	default:
	}
	m.watchCurrentTheme()
	assert.Empty(t, fw.runs)
}

func TestWatchCurrentTheme_ParentCancellation(t *testing.T) {
	m, _ := newTestModel(t)
	fw := setupThemeWatcher(t, m)
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	m.themeCtx = parent
	m.watchCurrentTheme()
	run := nextThemeWatchRun(t, fw)

	cancel()

	waitThemeWatchRun(t, run)
}

func TestHandleThemeFileChanged_IgnoresPreviousTheme(t *testing.T) {
	m, _ := newTestModel(t)
	current := styles.CurrentTheme()

	_, cmd := m.handleThemeFileChanged("user:previous-theme")

	assert.Nil(t, cmd)
	assert.Same(t, current, styles.CurrentTheme())
}

func TestSendThemeChange_CancelWithoutReceiver(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := &appModel{themeChanges: make(chan messages.ThemeFileChangedMsg)}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			m.sendThemeChange(ctx, "test")
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("delivery returned before cancellation")
		default:
		}
		cancel()
		<-done
	})
}

func TestWaitForThemeChange_Cancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	m := &appModel{themeCtx: ctx, themeChanges: make(chan messages.ThemeFileChangedMsg)}
	cmd := m.waitForThemeChange()
	cancel()
	assert.Nil(t, cmd())
}

func TestThemeChange_RearmsAfterIgnoredOrInvalidTheme(t *testing.T) {
	for _, ref := range []string{"user:previous-theme", "user:missing"} {
		t.Run(ref, func(t *testing.T) {
			m, _ := newTestModel(t)
			m.themeCtx = t.Context()
			m.themeChanges = make(chan messages.ThemeFileChangedMsg)
			if ref == "user:missing" {
				previous := styles.CurrentTheme()
				styles.ApplyTheme(&styles.Theme{Ref: ref})
				t.Cleanup(func() { styles.ApplyTheme(previous) })
			}
			_, cmd := m.Update(messages.ThemeFileChangedMsg{ThemeRef: ref})
			require.NotNil(t, cmd)
			go m.sendThemeChange(t.Context(), "next")
			msg := cmd()
			if batch, ok := msg.(tea.BatchMsg); ok {
				msg = batch[len(batch)-1]()
			}
			assert.Equal(t, messages.ThemeFileChangedMsg{ThemeRef: "next"}, msg)
		})
	}
}

func TestWatchCurrentTheme_WaitsForAttachment(t *testing.T) {
	m, _ := newTestModel(t)
	fw := setupThemeWatcher(t, m)
	attach := make(chan struct{})
	fw.attach = attach
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		m.watchCurrentTheme()
	}()
	nextThemeWatchRun(t, fw)
	select {
	case <-returned:
		t.Fatal("watchCurrentTheme returned before attachment")
	default:
	}
	close(attach)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("watchCurrentTheme did not return after attachment")
	}
}

func TestCleanupManagedResources_JoinsThemeWatcher(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t)
		fw := setupThemeWatcher(t, m)
		release := make(chan struct{})
		fw.release = release
		m.watchCurrentTheme()
		run := nextThemeWatchRun(t, fw)
		returned := make(chan struct{})
		go func() {
			defer close(returned)
			m.cleanupManagedResources()
		}()
		synctest.Wait()
		require.ErrorIs(t, run.ctx.Err(), context.Canceled)
		select {
		case <-returned:
			t.Fatal("cleanup returned before the watcher exited")
		default:
		}
		close(release)
		<-returned
		select {
		case <-run.done:
		default:
			t.Fatal("cleanup did not join the watcher")
		}
	})
}

func TestWatchCurrentTheme_JoinsPreviousRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t)
		fw := setupThemeWatcher(t, m)
		release := make(chan struct{})
		fw.release = release
		m.watchCurrentTheme()
		first := nextThemeWatchRun(t, fw)
		switchTheme(t, "dracula")
		returned := make(chan struct{})
		go func() {
			defer close(returned)
			m.watchCurrentTheme()
		}()
		synctest.Wait()
		require.ErrorIs(t, first.ctx.Err(), context.Canceled)
		assert.Empty(t, fw.runs, "replacement must wait for the previous run")
		close(release)
		<-returned
		second := nextThemeWatchRun(t, fw)
		require.NoError(t, second.ctx.Err())
	})
}

func TestCleanupManagedResources_UnblocksThemeNotification(t *testing.T) {
	m, _ := newTestModel(t)
	fw := setupThemeWatcher(t, m)
	m.themeChanges = make(chan messages.ThemeFileChangedMsg)
	fw.notify = m.sendThemeChange
	m.watchCurrentTheme()
	run := nextThemeWatchRun(t, fw)
	m.cleanupManagedResources()
	select {
	case <-run.done:
	default:
		t.Fatal("cleanup left a watcher blocked on notification delivery")
	}
}

func TestCleanupManagedResources_CancelsPendingAttachment(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t)
		fw := setupThemeWatcher(t, m)
		fw.attach = make(chan struct{})
		returned := make(chan struct{})
		go func() {
			defer close(returned)
			m.watchCurrentTheme()
		}()
		run := nextThemeWatchRun(t, fw)
		m.cleanupManagedResources()
		<-returned
		waitThemeWatchRun(t, run)
		m.watchCurrentTheme()
		assert.Empty(t, fw.runs)
	})
}

func TestWatchCurrentTheme_ReturnsWithoutReadiness(t *testing.T) {
	for _, err := range []error{nil, errors.New("watcher setup failed")} {
		t.Run(fmt.Sprint(err), func(t *testing.T) {
			m, _ := newTestModel(t)
			fw := setupThemeWatcher(t, m)
			fw.startupResult = func() error { return err }
			m.watchCurrentTheme()
			run := nextThemeWatchRun(t, fw)
			select {
			case <-run.done:
			default:
				t.Fatal("watchCurrentTheme returned before the no-op or failed run completed")
			}
		})
	}
}
