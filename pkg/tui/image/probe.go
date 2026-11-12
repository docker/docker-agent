package image

import (
	"bytes"
	"os"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/mattn/go-isatty"
	"golang.org/x/term"
)

const (
	kittyProbeID      = "4242"
	kittyProbeTimeout = 300 * time.Millisecond
	kittyProbeQuery   = "\x1b_Gi=" + kittyProbeID + ",a=q,t=d,f=24,s=1,v=1;AAAA\x1b\\"
	kittyProbeOK      = "\x1b_Gi=" + kittyProbeID + ";OK\x1b\\"
	// Terminals reply in query order and virtually all answer DA1, so its
	// reply ends the probe on terminals that ignore the graphics query.
	da1Query = "\x1b[c"
)

func kittyProbeResponse(response []byte) (supported, answered bool) {
	prefix := []byte("\x1b_Gi=" + kittyProbeID + ";")
	for {
		start := bytes.Index(response, prefix)
		if start < 0 {
			return false, false
		}
		response = response[start+len(prefix):]
		end := bytes.Index(response, []byte("\x1b\\"))
		if end < 0 {
			return false, false
		}
		if end > 0 {
			return bytes.Equal(response[:end], []byte("OK")), true
		}
		response = response[end+2:]
	}
}

// da1Answered reports whether response holds a complete DA1 reply (CSI ? Ps ; ... c).
func da1Answered(response []byte) bool {
	prefix := []byte("\x1b[?")
	for {
		start := bytes.Index(response, prefix)
		if start < 0 {
			return false
		}
		response = response[start+len(prefix):]
		end := 0
		for end < len(response) && (response[end] == ';' || (response[end] >= '0' && response[end] <= '9')) {
			end++
		}
		if end < len(response) && response[end] == 'c' {
			return true
		}
	}
}

// canProbeKittyGraphics reports whether both files are terminals.
func canProbeKittyGraphics(in, out *os.File) bool {
	return in != nil && out != nil && isatty.IsTerminal(in.Fd()) && isatty.IsTerminal(out.Fd())
}

// SupportsKittyGraphics requires exclusive ownership of terminal input.
func SupportsKittyGraphics(in, out *os.File) bool {
	if !canProbeKittyGraphics(in, out) {
		return false
	}

	state, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return false
	}
	defer func() { _ = term.Restore(int(in.Fd()), state) }()

	reader, err := uv.NewCancelReader(in)
	if err != nil {
		return false
	}
	defer reader.Close()

	timer := time.AfterFunc(kittyProbeTimeout, func() { reader.Cancel() })
	defer timer.Stop()
	if _, err := out.WriteString(kittyProbeQuery + da1Query); err != nil {
		return false
	}

	var supported, answered bool
	response := make([]byte, 0, 128)
	buf := make([]byte, 128)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			response = append(response, buf[:n]...)
			if !answered {
				supported, answered = kittyProbeResponse(response)
			}
			// Read through the DA1 reply so it never reaches the TUI as input.
			if da1Answered(response) {
				return supported
			}
			if len(response) > 1024 {
				response = response[len(response)-512:]
			}
		}
		if err != nil {
			return supported
		}
	}
}
