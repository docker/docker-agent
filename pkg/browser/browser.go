package browser

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
)

func Open(ctx context.Context, urlToOpen string) error {
	if err := validate(urlToOpen); err != nil {
		return err
	}

	var cmd string
	var args []string

	switch runtime.GOOS {
	case "windows":
		cmd = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", urlToOpen}
	case "darwin":
		cmd = "open"
		args = []string{urlToOpen}
	case "linux":
		cmd = "xdg-open"
		args = []string{urlToOpen}
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}

	command := exec.CommandContext(ctx, cmd, args...)
	if err := command.Start(); err != nil {
		return fmt.Errorf("failed to open browser: %w", err)
	}
	// Reap the child in the background so it does not linger as a zombie.
	go func() { _ = command.Wait() }()

	return nil
}

// validate rejects inputs that are unsafe to hand to the platform "open"
// helper (open, xdg-open, rundll32). Those helpers receive the URL as a
// positional argument, so a value beginning with "-" would be parsed as a
// command-line flag rather than a URL (argument injection). This matters now
// that URLs can come from agent configuration, which may be pulled from an
// untrusted registry.
//
// We require a parseable URL with a non-empty scheme so that option-like
// strings ("-foo", "--version") and bare paths are rejected. Any scheme the
// OS knows how to dispatch is allowed — including custom schemes such as
// "docker-desktop://" — since restricting to http(s) would defeat deep-link
// use cases.
func validate(raw string) error {
	if raw == "" {
		return errors.New("empty URL")
	}
	if strings.HasPrefix(raw, "-") {
		return fmt.Errorf("refusing to open URL that looks like a command-line flag: %q", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	if u.Scheme == "" {
		return fmt.Errorf("URL must include a scheme (e.g. https://): %q", raw)
	}
	return nil
}
