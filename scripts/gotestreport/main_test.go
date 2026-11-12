package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunBuffersInterleavedPackagesAndPreservesFailedOutput(t *testing.T) {
	t.Parallel()

	longOutput := strings.Repeat("x", 128*1024) + "\n"
	input := strings.Join([]string{
		eventJSON("start", "example/alpha", "", 0, ""),
		eventJSON("start", "example/beta", "", 0, ""),
		eventJSON("output", "example/alpha", "TestRoot", 0, "=== RUN   TestRoot\n"),
		eventJSON("output", "example/beta", "TestOK", 0, "=== RUN   TestOK\n"),
		eventJSON("output", "example/alpha", "TestRoot", 0, "--- FAIL: TestRoot (0.01s)\n"),
		eventJSON("output", "example/beta", "TestOK", 0, "--- PASS: TestOK (0.01s)\n"),
		eventJSON("output", "example/alpha", "TestRoot", 0, "    root_test.go:12: details\n"),
		eventJSON("output", "example/beta", "", 0, "PASS\n"),
		eventJSON("output", "example/alpha", "TestRoot", 0, longOutput),
		eventJSON("output", "example/beta", "", 0, "ok  \texample/beta\t0.10s\n"),
		eventJSON("pass", "example/beta", "TestOK", 0.01, ""),
		eventJSON("pass", "example/beta", "", 0.1, ""),
		eventJSON("output", "example/alpha", "TestPassing", 0, "=== RUN   TestPassing\n"),
		eventJSON("output", "example/alpha", "TestPassing", 0, "passing output\n"),
		eventJSON("output", "example/alpha", "TestPassing", 0, "--- PASS: TestPassing (0.01s)\n"),
		eventJSON("pass", "example/alpha", "TestPassing", 0.01, ""),
		eventJSON("fail", "example/alpha", "TestRoot", 0.01, ""),
		eventJSON("output", "example/alpha", "", 0, "FAIL\n"),
		eventJSON("output", "example/alpha", "", 0, "FAIL\texample/alpha\t1.25s\n"),
		eventJSON("fail", "example/alpha", "", 1.25, ""),
	}, "\n") + "\n"

	outPath := filepath.Join(t.TempDir(), "nested", "go-test.json")
	var live, summary bytes.Buffer
	err := run(strings.NewReader(input), &live, &summary, outPath, "")
	if !errors.Is(err, errTestsFailed) {
		t.Fatalf("run() error = %v, want tests failed", err)
	}

	raw, readErr := os.ReadFile(outPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(raw) != input {
		t.Fatal("raw event file did not preserve the input verbatim")
	}

	betaOutput := "PASS\nok  \texample/beta\t0.10s\n"
	alphaOutput := "=== RUN   TestRoot\n--- FAIL: TestRoot (0.01s)\n    root_test.go:12: details\n" + longOutput + "FAIL\nFAIL\texample/alpha\t1.25s\n"
	if want := betaOutput + alphaOutput; live.String() != want {
		t.Fatalf("live output = %q, want %q", live.String(), want)
	}
	if strings.Contains(live.String(), "TestOK") || strings.Contains(live.String(), "TestPassing") || strings.Contains(live.String(), "passing output") {
		t.Fatalf("live output retained passing test noise: %q", live.String())
	}
	failedAt := strings.Index(live.String(), "--- FAIL: TestRoot (0.01s)\n")
	if failedAt < 0 || (failedAt > 0 && live.String()[failedAt-1] != '\n') {
		t.Fatalf("root failure is not at column 0: %q", live.String())
	}
	if !strings.HasPrefix(live.String()[failedAt:], "--- FAIL: TestRoot (0.01s)\n    root_test.go:12: details\n") {
		t.Fatalf("root failure details are not contiguous: %q", live.String())
	}
	if !strings.Contains(live.String(), longOutput) {
		t.Fatal("live output omitted the event larger than Scanner's default token limit")
	}
	for _, want := range []string{"Status: **failed**", "`example/alpha: TestRoot`", "1.25s", "`example/alpha`"} {
		if !strings.Contains(summary.String(), want) {
			t.Errorf("summary missing %q:\n%s", want, summary.String())
		}
	}
}

func TestRunGroupsInterleavedParallelRootFailures(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		eventJSON("start", "example/parallel", "", 0, ""),
		eventJSON("run", "example/parallel", "TestFirst", 0, ""),
		eventJSON("output", "example/parallel", "TestFirst", 0, "=== RUN   TestFirst\n"),
		eventJSON("output", "example/parallel", "TestFirst", 0, "=== PAUSE TestFirst\n"),
		eventJSON("pause", "example/parallel", "TestFirst", 0, ""),
		eventJSON("run", "example/parallel", "TestSecond", 0, ""),
		eventJSON("output", "example/parallel", "TestSecond", 0, "=== RUN   TestSecond\n"),
		eventJSON("output", "example/parallel", "TestSecond", 0, "=== PAUSE TestSecond\n"),
		eventJSON("pause", "example/parallel", "TestSecond", 0, ""),
		eventJSON("cont", "example/parallel", "TestFirst", 0, ""),
		eventJSON("output", "example/parallel", "TestFirst", 0, "=== CONT  TestFirst\n"),
		eventJSON("run", "example/parallel", "TestFirst/child", 0, ""),
		eventJSON("output", "example/parallel", "TestFirst/child", 0, "=== RUN   TestFirst/child\n"),
		eventJSON("cont", "example/parallel", "TestSecond", 0, ""),
		eventJSON("output", "example/parallel", "TestSecond", 0, "=== CONT  TestSecond\n"),
		eventJSON("run", "example/parallel", "TestSecond/child", 0, ""),
		eventJSON("output", "example/parallel", "TestSecond/child", 0, "=== RUN   TestSecond/child\n"),
		eventJSON("output", "example/parallel", "TestFirst/child", 0, "    parallel_test.go:10: first diagnostic\n"),
		eventJSON("output", "example/parallel", "TestSecond/child", 0, "    parallel_test.go:20: second diagnostic\n"),
		eventJSON("output", "example/parallel", "TestFirst", 0, "--- FAIL: TestFirst (0.01s)\n"),
		eventJSON("output", "example/parallel", "TestFirst/child", 0, "    --- FAIL: TestFirst/child (0.01s)\n"),
		eventJSON("fail", "example/parallel", "TestFirst/child", 0.01, ""),
		eventJSON("fail", "example/parallel", "TestFirst", 0.01, ""),
		eventJSON("output", "example/parallel", "TestSecond", 0, "--- FAIL: TestSecond (0.02s)\n"),
		eventJSON("output", "example/parallel", "TestSecond/child", 0, "    --- FAIL: TestSecond/child (0.02s)\n"),
		eventJSON("fail", "example/parallel", "TestSecond/child", 0.02, ""),
		eventJSON("fail", "example/parallel", "TestSecond", 0.02, ""),
		eventJSON("output", "example/parallel", "", 0, "FAIL\n"),
		eventJSON("output", "example/parallel", "", 0, "FAIL\texample/parallel\t0.03s\n"),
		eventJSON("fail", "example/parallel", "", 0.03, ""),
	}, "\n") + "\n"

	outPath := filepath.Join(t.TempDir(), "go-test.json")
	var live, summary bytes.Buffer
	err := run(strings.NewReader(input), &live, &summary, outPath, "")
	if !errors.Is(err, errTestsFailed) {
		t.Fatalf("run() error = %v, want tests failed", err)
	}

	raw, readErr := os.ReadFile(outPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(raw) != input {
		t.Fatal("raw event file did not preserve the input verbatim")
	}

	first := "=== RUN   TestFirst\n=== PAUSE TestFirst\n=== CONT  TestFirst\n=== RUN   TestFirst/child\n    parallel_test.go:10: first diagnostic\n--- FAIL: TestFirst (0.01s)\n    --- FAIL: TestFirst/child (0.01s)\n"
	second := "=== RUN   TestSecond\n=== PAUSE TestSecond\n=== CONT  TestSecond\n=== RUN   TestSecond/child\n    parallel_test.go:20: second diagnostic\n--- FAIL: TestSecond (0.02s)\n    --- FAIL: TestSecond/child (0.02s)\n"
	if want := first + second + "FAIL\nFAIL\texample/parallel\t0.03s\n"; live.String() != want {
		t.Fatalf("live output = %q, want %q", live.String(), want)
	}
	for root, diagnostics := range map[string][2]string{
		"TestFirst":  {"first diagnostic", "second diagnostic"},
		"TestSecond": {"second diagnostic", "first diagnostic"},
	} {
		own, other := diagnostics[0], diagnostics[1]
		blockStart := strings.Index(live.String(), "=== RUN   "+root+"\n")
		marker := "--- FAIL: " + root
		markerAt := strings.Index(live.String(), marker)
		if blockStart < 0 || markerAt < blockStart {
			t.Errorf("%s block is missing its root marker:\n%s", root, live.String())
			continue
		}
		blockEnd := len(live.String())
		if next := strings.Index(live.String()[markerAt:], "\n=== RUN   Test"); next >= 0 {
			blockEnd = markerAt + next + 1
		}
		block := live.String()[blockStart:blockEnd]
		if !strings.Contains(block, own) || strings.Contains(block, other) {
			t.Errorf("%s block has misattributed diagnostics: %q", root, block)
		}
	}
}

func TestRunPreservesPackageFailureWithoutTestFailure(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		eventJSON("start", "example/timeout", "", 0, ""),
		eventJSON("run", "example/timeout", "TestPassed", 0, ""),
		eventJSON("output", "example/timeout", "TestPassed", 0, "=== RUN   TestPassed\n"),
		eventJSON("output", "example/timeout", "TestPassed", 0, "passing output\n"),
		eventJSON("output", "example/timeout", "TestPassed", 0, "--- PASS: TestPassed (0.01s)\n"),
		eventJSON("pass", "example/timeout", "TestPassed", 0.01, ""),
		eventJSON("run", "example/timeout", "TestSkipped", 0, ""),
		eventJSON("output", "example/timeout", "TestSkipped", 0, "=== RUN   TestSkipped\n"),
		eventJSON("output", "example/timeout", "TestSkipped", 0, "--- SKIP: TestSkipped (0.01s)\n"),
		eventJSON("skip", "example/timeout", "TestSkipped", 0.01, ""),
		eventJSON("run", "example/timeout", "TestHung", 0, ""),
		eventJSON("output", "example/timeout", "TestHung", 0, "=== RUN   TestHung\n"),
		eventJSON("output", "example/timeout", "TestHung", 0, "panic: test timed out after 10m0s\n"),
		eventJSON("output", "example/timeout", "TestHung", 0, "goroutine 7 [running]:\n"),
		eventJSON("output", "example/timeout", "", 0, "FAIL\texample/timeout\t600.00s\n"),
		eventJSON("fail", "example/timeout", "", 600, ""),
	}, "\n") + "\n"

	outPath := filepath.Join(t.TempDir(), "go-test.json")
	var live, summary bytes.Buffer
	err := run(strings.NewReader(input), &live, &summary, outPath, "")
	if !errors.Is(err, errTestsFailed) {
		t.Fatalf("run() error = %v, want tests failed", err)
	}

	raw, readErr := os.ReadFile(outPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(raw) != input {
		t.Fatal("raw event file did not preserve the input verbatim")
	}

	want := "=== RUN   TestHung\npanic: test timed out after 10m0s\ngoroutine 7 [running]:\nFAIL\texample/timeout\t600.00s\n"
	if live.String() != want {
		t.Fatalf("live output = %q, want %q", live.String(), want)
	}
	if strings.Contains(live.String(), "TestPassed") || strings.Contains(live.String(), "passing output") || strings.Contains(live.String(), "TestSkipped") {
		t.Fatalf("live output retained passing or skipped test noise: %q", live.String())
	}
}

func TestRunPreservesBackgroundPanicWithoutTestFailure(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		eventJSON("start", "example/panic", "", 0, ""),
		eventJSON("run", "example/panic", "TestReturns", 0, ""),
		eventJSON("output", "example/panic", "TestReturns", 0, "=== RUN   TestReturns\n"),
		eventJSON("output", "example/panic", "TestReturns", 0, "panic: background boom\n"),
		eventJSON("output", "example/panic", "TestReturns", 0, "created by example/panic.TestReturns\n"),
		eventJSON("output", "example/panic", "", 0, "FAIL\n"),
		eventJSON("fail", "example/panic", "", 0.03, ""),
	}, "\n") + "\n"

	outPath := filepath.Join(t.TempDir(), "go-test.json")
	var live, summary bytes.Buffer
	err := run(strings.NewReader(input), &live, &summary, outPath, "")
	if !errors.Is(err, errTestsFailed) {
		t.Fatalf("run() error = %v, want tests failed", err)
	}

	want := "=== RUN   TestReturns\npanic: background boom\ncreated by example/panic.TestReturns\nFAIL\n"
	if live.String() != want {
		t.Fatalf("live output = %q, want %q", live.String(), want)
	}
}

func TestRunFlushesTruncatedPackagesAtEOF(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		eventJSON("start", "example/zeta", "", 0, ""),
		eventJSON("output", "example/zeta", "TestPending", 0, "=== RUN   TestPending\n"),
		eventJSON("output", "example/zeta", "TestPending", 0, "pending diagnostic\n"),
		eventJSON("start", "example/alpha", "", 0, ""),
		eventJSON("output", "example/alpha", "TestFailed", 0, "=== RUN   TestFailed\n"),
		eventJSON("output", "example/alpha", "TestFailed", 0, "--- FAIL: TestFailed (0.01s)\n"),
		eventJSON("output", "example/alpha", "TestFailed", 0, "    failed_test.go:12: details\n"),
		eventJSON("fail", "example/alpha", "TestFailed", 0.01, ""),
	}, "\n")

	outPath := filepath.Join(t.TempDir(), "go-test.json")
	var live, summary bytes.Buffer
	err := run(strings.NewReader(input), &live, &summary, outPath, "")
	if !errors.Is(err, errTestsFailed) {
		t.Fatalf("run() error = %v, want tests failed for a truncated stream", err)
	}

	raw, readErr := os.ReadFile(outPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(raw) != input {
		t.Fatal("raw event file did not preserve the unterminated input verbatim")
	}

	want := "=== RUN   TestFailed\n--- FAIL: TestFailed (0.01s)\n    failed_test.go:12: details\n=== RUN   TestPending\npending diagnostic\n"
	if live.String() != want {
		t.Fatalf("live output = %q, want %q", live.String(), want)
	}
	if !strings.Contains(summary.String(), "Status: **failed**") {
		t.Fatalf("summary = %q, want failed status", summary.String())
	}
}

func TestRunWritesStepSummaryAndReturnsParseErrors(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		eventJSON("pass", "example/ok", "TestOK", 0.25, "ok output\n"),
		eventJSON("output", "example/ok", "", 0, "ok  \texample/ok\t0.25s\n"),
		eventJSON("pass", "example/ok", "", 0.25, ""),
		"not-json",
	}, "\n") + "\n"
	dir := t.TempDir()
	outPath := filepath.Join(dir, "go-test.json")
	summaryPath := filepath.Join(dir, "summary.md")
	var live, fallback bytes.Buffer

	err := run(strings.NewReader(input), &live, &fallback, outPath, summaryPath)
	if err == nil || !strings.Contains(err.Error(), "decode event on line 4") {
		t.Fatalf("run() error = %v, want line-numbered decode error", err)
	}
	if fallback.Len() != 0 {
		t.Fatalf("fallback summary = %q, want empty", fallback.String())
	}
	summary, readErr := os.ReadFile(summaryPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(summary), "Status: **passed; 1 malformed event(s)**") {
		t.Fatalf("summary = %q", summary)
	}
	if live.String() != "ok  \texample/ok\t0.25s\nnot-json\n" {
		t.Fatalf("live output = %q", live.String())
	}
}

func TestReportIncludesOnlyTerminalTimingsAndSortsThem(t *testing.T) {
	t.Parallel()

	var result report
	result.add(testEvent{Action: "run", Package: "example/run", Test: "TestRunning", Elapsed: 99})
	for i := range reportLimit + 2 {
		result.add(testEvent{
			Action:  "pass",
			Package: "example/tests",
			Test:    fmt.Sprintf("Test%02d", i),
			Elapsed: float64(i + 1),
		})
	}
	result.add(testEvent{Action: "skip", Package: "example/pkg", Elapsed: 2})

	var summary bytes.Buffer
	if err := writeSummary(result, "", &summary); err != nil {
		t.Fatal(err)
	}
	text := summary.String()
	if strings.Contains(text, "TestRunning") || strings.Contains(text, "Test00") || strings.Contains(text, "Test01") {
		t.Fatalf("summary contains excluded timing:\n%s", text)
	}
	if first, second := strings.Index(text, "Test11"), strings.Index(text, "Test10"); first < 0 || second < 0 || first > second {
		t.Fatalf("tests are not sorted from slowest to fastest:\n%s", text)
	}
	if !strings.Contains(text, "`example/pkg`") {
		t.Fatalf("package timing missing:\n%s", text)
	}
}

func TestProcessReportsWriteFailure(t *testing.T) {
	t.Parallel()

	_, err := process(strings.NewReader(eventJSON("pass", "example/ok", "", 1, "")+"\n"), failingWriter{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "write raw event stream") {
		t.Fatalf("process() error = %v", err)
	}
}

func TestProcessReportsLiveOutputWriteFailure(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"package terminal": eventJSON("output", "example/fail", "TestRoot", 0, "--- FAIL: TestRoot (0.01s)\n") + "\n" +
			eventJSON("fail", "example/fail", "TestRoot", 0.01, "") + "\n" +
			eventJSON("fail", "example/fail", "", 0.01, "") + "\n",
		"end of stream": eventJSON("output", "example/truncated", "TestRoot", 0, "--- FAIL: TestRoot (0.01s)\n") + "\n",
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := process(strings.NewReader(input), &bytes.Buffer{}, failingWriter{})
			if err == nil || !strings.Contains(err.Error(), "write live test output") {
				t.Fatalf("process() error = %v", err)
			}
		})
	}
}

func eventJSON(action, pkg, test string, elapsed float64, output string) string {
	return fmt.Sprintf(`{"Action":%q,"Package":%q,"Test":%q,"Elapsed":%g,"Output":%q}`, action, pkg, test, elapsed, output)
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}
