//go:build !windows

package root

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPlansCreate_RejectsNamedPipe proves a non-regular --file (here a named
// pipe with no writer) is rejected without hanging and without being read.
// The pipe is opened with O_NONBLOCK — a plain open would block forever
// waiting for a writer — and then refused when the opened descriptor turns
// out not to be a regular file. The timeout guards against a regression to a
// blocking open.
func TestPlansCreate_RejectsNamedPipe(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)

	fifo := filepath.Join(t.TempDir(), "content.pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot create a FIFO on this system: %v", err)
	}

	type result struct {
		stdout, stderr string
		err            error
	}
	done := make(chan result, 1)
	go func() {
		stdout, stderr, err := executePlans(t, svc, "create", "p", "--file", fifo, "--json")
		done <- result{stdout, stderr, err}
	}()

	select {
	case res := <-done:
		requirePlansStatusCode(t, res.err, 1)
		assert.Empty(t, res.stdout)
		body := decodePlansError(t, res.stderr)
		assert.Equal(t, "invalid_argument", body.Code)
		assert.Contains(t, body.Message, "not a regular file")
	case <-time.After(5 * time.Second):
		t.Fatal("create blocked on a named pipe; the open must not block and the descriptor must be rejected before any read")
	}
}

// TestPlansCreate_RejectsDevice proves a device --file is rejected on the
// opened descriptor without being read: reading /dev/zero would stream
// unbounded data and trip the size cap at best, so the distinct "not a
// regular file" message shows no read happened.
func TestPlansCreate_RejectsDevice(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)

	if _, err := os.Stat("/dev/zero"); err != nil {
		t.Skipf("/dev/zero not available: %v", err)
	}

	stdout, stderr, err := executePlans(t, svc, "create", "p", "--file", "/dev/zero", "--json")
	requirePlansStatusCode(t, err, 1)
	assert.Empty(t, stdout)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "invalid_argument", body.Code)
	assert.Contains(t, body.Message, "not a regular file")
}

// TestPlansGetList_FIFOPlanFileFailsFast proves a FIFO squatting on a
// stored plan file cannot wedge the read commands: get reports it as a
// typed corrupt plan and list degrades it to a warning while still listing
// healthy plans — both promptly. The timeout guards against a regression to
// a blocking open in the storage's load path.
func TestPlansGetList_FIFOPlanFileFailsFast(t *testing.T) {
	t.Parallel()
	svc, sharedDir := newPlansTestService(t)
	mustCreatePlan(t, svc, "good", "content")

	if err := syscall.Mkfifo(filepath.Join(sharedDir, "wedged.json"), 0o600); err != nil {
		t.Skipf("cannot create a FIFO on this system: %v", err)
	}

	type result struct {
		stdout, stderr string
		err            error
	}

	getDone := make(chan result, 1)
	go func() {
		stdout, stderr, err := executePlans(t, svc, "get", "wedged", "--json")
		getDone <- result{stdout, stderr, err}
	}()
	select {
	case res := <-getDone:
		requirePlansStatusCode(t, res.err, 1)
		assert.Empty(t, res.stdout)
		body := decodePlansError(t, res.stderr)
		assert.Equal(t, "corrupt", body.Code)
		assert.Equal(t, "wedged", body.Name)
		assert.Contains(t, body.Message, "not a regular file")
	case <-time.After(5 * time.Second):
		t.Fatal("plans get blocked on a FIFO plan file; the open must not block and the descriptor must be rejected before any read")
	}

	listDone := make(chan result, 1)
	go func() {
		stdout, stderr, err := executePlans(t, svc, "list", "--json")
		listDone <- result{stdout, stderr, err}
	}()
	select {
	case res := <-listDone:
		require.NoError(t, res.err)
		assert.Contains(t, res.stdout, `"name": "good"`, "the healthy plan must still be listed")
		assert.NotContains(t, res.stdout, `"name": "wedged"`)
		assert.Contains(t, res.stdout, "warnings", "the FIFO must surface as a warning")
		assert.Contains(t, res.stdout, "wedged")
	case <-time.After(5 * time.Second):
		t.Fatal("plans list blocked on a FIFO plan file; the open must not block and the descriptor must be rejected before any read")
	}
}
