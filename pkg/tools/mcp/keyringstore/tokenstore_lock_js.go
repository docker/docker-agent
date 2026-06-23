//go:build js && wasm

package keyringstore

import "os"

func lockExclusive(_ *os.File) error { return nil }

func unlockFile(_ *os.File) error { return nil }
