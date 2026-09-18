package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const reportLimit = 10

var errTestsFailed = errors.New("tests failed")

type testEvent struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Elapsed float64 `json:"Elapsed"`
	Output  string  `json:"Output"`
}

type timing struct {
	name    string
	elapsed float64
}

type outputEvent struct {
	test   string
	output string
}

type outputGroup struct {
	root   string
	events []outputEvent
}

type packageOutput struct {
	groups         []outputGroup
	groupIndexes   map[string]int
	completedTests map[string]struct{}
}

type report struct {
	tests       []timing
	packages    []timing
	failed      bool
	parseErrors []error
}

func main() {
	out := flag.String("out", "", "file for the raw go test -json event stream")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "gotestreport: -out is required")
		os.Exit(2)
	}

	if err := run(os.Stdin, os.Stdout, os.Stderr, *out, os.Getenv("GITHUB_STEP_SUMMARY")); err != nil {
		fmt.Fprintf(os.Stderr, "gotestreport: %v\n", err)
		os.Exit(1)
	}
}

func run(input io.Reader, liveOutput, fallbackSummary io.Writer, outPath, summaryPath string) error {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o750); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	out, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create raw event file: %w", err)
	}

	report, processErr := process(input, out, liveOutput)
	closeErr := out.Close()
	summaryErr := writeSummary(report, summaryPath, fallbackSummary)

	var failures []error
	if processErr != nil {
		failures = append(failures, processErr)
	}
	if closeErr != nil {
		failures = append(failures, fmt.Errorf("close raw event file: %w", closeErr))
	}
	if summaryErr != nil {
		failures = append(failures, summaryErr)
	}
	if report.failed {
		failures = append(failures, errTestsFailed)
	}
	return errors.Join(failures...)
}

func process(input io.Reader, rawOutput, liveOutput io.Writer) (report, error) {
	reader := bufio.NewReader(input)
	packageOutputs := make(map[string]*packageOutput)
	var result report
	lineNumber := 0

	for {
		line, readErr := reader.ReadString('\n')
		if line != "" {
			lineNumber++
			if _, err := io.WriteString(rawOutput, line); err != nil {
				return result, fmt.Errorf("write raw event stream: %w", err)
			}
			var event testEvent
			if err := json.Unmarshal([]byte(strings.TrimSuffix(line, "\n")), &event); err != nil {
				result.parseErrors = append(result.parseErrors, fmt.Errorf("decode event on line %d: %w", lineNumber, err))
				if _, writeErr := io.WriteString(liveOutput, line); writeErr != nil {
					return result, fmt.Errorf("write live test output: %w", writeErr)
				}
			} else {
				result.add(event)
				if event.Package == "" {
					if _, err := io.WriteString(liveOutput, event.Output); err != nil {
						return result, fmt.Errorf("write live test output: %w", err)
					}
				} else {
					buffer := packageOutputs[event.Package]
					if buffer == nil {
						buffer = &packageOutput{
							groupIndexes:   make(map[string]int),
							completedTests: make(map[string]struct{}),
						}
						packageOutputs[event.Package] = buffer
					}
					if event.Output != "" {
						buffer.addOutput(event.Test, event.Output)
					}
					if (event.Action == "pass" || event.Action == "skip") && event.Test != "" {
						buffer.completedTests[event.Test] = struct{}{}
					}
				}
				if isPackageTerminal(event) {
					if err := flushPackageOutput(liveOutput, packageOutputs[event.Package], event.Action == "fail"); err != nil {
						return result, err
					}
					delete(packageOutputs, event.Package)
				}
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return result, fmt.Errorf("read event stream: %w", readErr)
			}
			break
		}
	}

	if len(packageOutputs) > 0 {
		result.failed = true
		packages := make([]string, 0, len(packageOutputs))
		for pkg := range packageOutputs {
			packages = append(packages, pkg)
		}
		sort.Strings(packages)
		for _, pkg := range packages {
			if err := flushPackageOutput(liveOutput, packageOutputs[pkg], true); err != nil {
				return result, err
			}
		}
	}

	return result, errors.Join(result.parseErrors...)
}

func isPackageTerminal(event testEvent) bool {
	if event.Test != "" {
		return false
	}
	return event.Action == "pass" || event.Action == "fail" || event.Action == "skip"
}

func (p *packageOutput) addOutput(test, output string) {
	root := test
	if slash := strings.IndexByte(root, '/'); slash >= 0 {
		root = root[:slash]
	}
	index, ok := p.groupIndexes[root]
	if !ok {
		index = len(p.groups)
		p.groupIndexes[root] = index
		p.groups = append(p.groups, outputGroup{root: root})
	}
	p.groups[index].events = append(p.groups[index].events, outputEvent{test: test, output: output})
}

func flushPackageOutput(output io.Writer, packageOutput *packageOutput, failed bool) error {
	if packageOutput == nil {
		return nil
	}
	for _, group := range packageOutput.groups {
		for _, event := range group.events {
			_, completed := packageOutput.completedTests[event.test]
			if group.root != "" && (!failed || completed) {
				continue
			}
			if _, err := io.WriteString(output, event.output); err != nil {
				return fmt.Errorf("write live test output: %w", err)
			}
		}
	}
	return nil
}

func (r *report) add(event testEvent) {
	if event.Action == "fail" {
		r.failed = true
	}
	if event.Elapsed <= 0 || (event.Action != "pass" && event.Action != "fail" && event.Action != "skip") {
		return
	}
	if event.Test != "" {
		r.tests = append(r.tests, timing{name: event.Package + ": " + event.Test, elapsed: event.Elapsed})
		return
	}
	if event.Package != "" {
		r.packages = append(r.packages, timing{name: event.Package, elapsed: event.Elapsed})
	}
}

func writeSummary(result report, summaryPath string, fallback io.Writer) error {
	output := fallback
	var file *os.File
	if summaryPath != "" {
		var err error
		file, err = os.OpenFile(summaryPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return fmt.Errorf("open GitHub step summary: %w", err)
		}
		defer file.Close()
		output = file
	}

	status := "passed"
	if result.failed {
		status = "failed"
	}
	if len(result.parseErrors) > 0 {
		status += fmt.Sprintf("; %d malformed event(s)", len(result.parseErrors))
	}
	if _, err := fmt.Fprintf(output, "## Go test timing\n\nStatus: **%s**\n\n", status); err != nil {
		return fmt.Errorf("write test summary: %w", err)
	}
	if err := writeTimings(output, "Slowest tests", result.tests); err != nil {
		return err
	}
	if err := writeTimings(output, "Slowest packages", result.packages); err != nil {
		return err
	}
	return nil
}

func writeTimings(output io.Writer, heading string, timings []timing) error {
	sort.SliceStable(timings, func(i, j int) bool {
		return timings[i].elapsed > timings[j].elapsed
	})
	if len(timings) > reportLimit {
		timings = timings[:reportLimit]
	}

	if _, err := fmt.Fprintf(output, "### %s\n\n", heading); err != nil {
		return fmt.Errorf("write test summary: %w", err)
	}
	if len(timings) == 0 {
		_, err := fmt.Fprint(output, "No timing data.\n\n")
		if err != nil {
			return fmt.Errorf("write test summary: %w", err)
		}
		return nil
	}
	if _, err := fmt.Fprint(output, "| Name | Duration |\n| --- | ---: |\n"); err != nil {
		return fmt.Errorf("write test summary: %w", err)
	}
	for _, item := range timings {
		name := strings.ReplaceAll(item.name, "|", "\\|")
		if _, err := fmt.Fprintf(output, "| `%s` | %.2fs |\n", name, item.elapsed); err != nil {
			return fmt.Errorf("write test summary: %w", err)
		}
	}
	if _, err := fmt.Fprintln(output); err != nil {
		return fmt.Errorf("write test summary: %w", err)
	}
	return nil
}
