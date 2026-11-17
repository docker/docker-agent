package markdowntransform

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
)

const helperModeEnv = "MARKDOWNTRANSFORM_TEST_HELPER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperModeEnv); mode != "" {
		serveHelper(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// serveHelper is a JSON-RPC transformer run as a subprocess of the tests.
// Results are prefixed with the PID so tests can observe restarts.
func serveHelper(mode string) {
	if mode == "exit" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(nil, maxLineBytes)
	out := json.NewEncoder(os.Stdout)
	for requests := 0; scanner.Scan(); requests++ {
		var request struct {
			ID     uint64 `json:"id"`
			Params Input  `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			continue
		}
		switch {
		case mode == "hang":
		case mode == "error-once" && requests == 0:
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": 1, "message": "rejected"}})
		default:
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "method": "notice"})
			markdown := fmt.Sprintf("%d:%s", os.Getpid(), request.Params.Markdown)
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]string{"markdown": markdown}})
		}
	}
}

func helperConfig(t *testing.T, mode string, timeoutMS int) Config {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := "exec '" + strings.ReplaceAll(executable, "'", `'\''`) + "'"
	if runtime.GOOS == "windows" {
		command = "& '" + strings.ReplaceAll(executable, "'", "''") + "'"
	}
	return Config{Command: command, TimeoutMS: timeoutMS, Env: map[string]string{helperModeEnv: mode}}
}
