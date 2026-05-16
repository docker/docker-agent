package evaluation

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/term"
)

// progressBar provides a live-updating progress display for evaluation runs.
type progressBar struct {
	ttyOut          io.Writer // output for progress bar rendering (TTY only)
	resultOut       io.Writer // output for results (can be tee'd to log)
	fd              int       // file descriptor for terminal size queries
	total           int
	completed       atomic.Int32
	passed          atomic.Int32
	failed          atomic.Int32
	relevanceFailed atomic.Int32 // count of evals with relevance failures
	sizeFailed      atomic.Int32 // count of evals with size failures
	toolCallsFailed atomic.Int32 // count of evals with tool call failures
	running         sync.Map     // map[string]bool for currently running evals
	done            chan struct{}
	stopped         chan struct{} // signals that the goroutine has finished
	ticker          *time.Ticker
	isTTY           bool
	mu              sync.Mutex // protects output
}

func newProgressBar(ttyOut, resultOut io.Writer, fd, total int, isTTY bool) *progressBar {
	return &progressBar{
		ttyOut:    ttyOut,
		resultOut: resultOut,
		fd:        fd,
		total:     total,
		done:      make(chan struct{}),
		stopped:   make(chan struct{}),
		isTTY:     isTTY,
	}
}

func (p *progressBar) start() {
	p.ticker = time.NewTicker(100 * time.Millisecond)
	go func() {
		defer close(p.stopped)
		for {
			select {
			case <-p.done:
				p.ticker.Stop()
				p.render(true)
				return
			case <-p.ticker.C:
				p.render(false)
			}
		}
	}()
}

// stop signals the progress bar to stop and waits for it to finish.
func (p *progressBar) stop() {
	close(p.done)
	<-p.stopped // wait for goroutine to finish
}

func (p *progressBar) setRunning(title string) {
	p.running.Store(title, true)
}

func (p *progressBar) complete(title string, success bool) {
	p.running.Delete(title)
	p.completed.Add(1)
	if success {
		p.passed.Add(1)
	} else {
		p.failed.Add(1)
	}
}

func (p *progressBar) printResult(result Result) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Clear current line on TTY
	if p.isTTY {
		fmt.Fprint(p.ttyOut, "\r\x1b[K")
	}

	successes, failures := result.checkResults()
	success := len(failures) == 0

	// Track failure categories
	if !success {
		for _, f := range failures {
			switch {
			case strings.HasPrefix(f, "relevance"):
				p.relevanceFailed.Add(1)
			case strings.HasPrefix(f, "size"):
				p.sizeFailed.Add(1)
			case strings.HasPrefix(f, "tool calls"):
				p.toolCallsFailed.Add(1)
			}
		}
	}

	// Print session title with icon (to result output, which may be tee'd to log)
	if success {
		fmt.Fprintf(p.resultOut, "%s %s ($%.6f)\n", p.green("✓"), result.Title, result.Cost)
	} else {
		fmt.Fprintf(p.resultOut, "%s %s ($%.6f)\n", p.red("✗"), result.Title, result.Cost)
	}

	// Print successes and failures
	for _, s := range successes {
		fmt.Fprintf(p.resultOut, "  %s %s\n", p.green("✓"), s)
	}
	for _, f := range failures {
		fmt.Fprintf(p.resultOut, "  %s %s\n", p.red("✗"), f)
	}
}

func (p *progressBar) green(s string) string {
	if p.isTTY {
		return "\x1b[32m" + s + "\x1b[0m"
	}
	return s
}

func (p *progressBar) red(s string) string {
	if p.isTTY {
		return "\x1b[31m" + s + "\x1b[0m"
	}
	return s
}

// getTerminalWidth returns the current terminal width, or a default if unavailable.
func (p *progressBar) getTerminalWidth() int {
	if !p.isTTY {
		return 80
	}
	width, _, err := term.GetSize(p.fd)
	if err != nil || width <= 0 {
		return 80
	}
	return width
}

func (p *progressBar) render(final bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	completed := int(p.completed.Load())
	passed := int(p.passed.Load())
	failed := int(p.failed.Load())
	relevanceFailed := int(p.relevanceFailed.Load())
	sizeFailed := int(p.sizeFailed.Load())
	toolCallsFailed := int(p.toolCallsFailed.Load())

	// Get current terminal width for dynamic sizing
	termWidth := p.getTerminalWidth()

	// Calculate bar width based on terminal size
	// Reserve space for: "[" + "]" + " 100% (999/999) " + counts (~20) + running info (~30)
	barWidth := min(max(termWidth-80, 10), 10)

	filledWidth := 0
	if p.total > 0 {
		filledWidth = (completed * barWidth) / p.total
	}

	bar := strings.Repeat("█", filledWidth) + strings.Repeat("░", barWidth-filledWidth)
	percent := 0
	if p.total > 0 {
		percent = (completed * 100) / p.total
	}

	// Count running evals
	runningCount := 0
	var firstName string
	p.running.Range(func(key, _ any) bool {
		runningCount++
		if firstName == "" {
			firstName = key.(string)
		}
		return true
	})

	// Build status line
	counts := fmt.Sprintf("%s %s", p.green(fmt.Sprintf("✓%d", passed)), p.red(fmt.Sprintf("✗%d", failed)))

	// Add detailed failure breakdown if there are failures (show during run, not just at end)
	if failed > 0 {
		breakdown := []string{}
		if relevanceFailed > 0 {
			breakdown = append(breakdown, "relv "+p.red(fmt.Sprintf("✗%d", relevanceFailed)))
		}
		if sizeFailed > 0 {
			breakdown = append(breakdown, "size "+p.red(fmt.Sprintf("✗%d", sizeFailed)))
		}
		if toolCallsFailed > 0 {
			breakdown = append(breakdown, "tools "+p.red(fmt.Sprintf("✗%d", toolCallsFailed)))
		}
		if len(breakdown) > 0 {
			counts += fmt.Sprintf(" (%s)", strings.Join(breakdown, ", "))
		}
	}

	status := fmt.Sprintf("[%s] %3d%% (%d/%d) %s", bar, percent, completed, p.total, counts)

	if runningCount > 0 {
		// Calculate available space for running task name
		availableForName := max(termWidth-len(status)-10, 5)
		name := firstName
		if len(name) > availableForName {
			name = name[:availableForName-1] + "…"
		}
		if runningCount == 1 {
			status += " | " + name
		} else {
			status += fmt.Sprintf(" | %s +%d more", name, runningCount-1)
		}
	}

	if p.isTTY {
		// Clear entire line and write status (to TTY only)
		fmt.Fprintf(p.ttyOut, "\r\x1b[K%s", status)
		if final {
			fmt.Fprintln(p.ttyOut)
		}
	} else if final {
		fmt.Fprintln(p.resultOut, status)
	}
}
