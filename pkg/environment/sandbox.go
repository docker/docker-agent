package environment

import "os"

// sandboxVMIDEnv is also read directly by pkg/sqliteutil (journalMode) to
// avoid a heavy import; update both sites if the detection signal changes.
const sandboxVMIDEnv = "SANDBOX_VM_ID"

// InSandbox reports whether docker agent is running inside a Docker sandbox.
// Detection relies on the SANDBOX_VM_ID environment variable that Docker
// Desktop sets in every sandbox VM.
func InSandbox() bool {
	return os.Getenv(sandboxVMIDEnv) != ""
}
