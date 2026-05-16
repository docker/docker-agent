package environment

import (
	"fmt"
	"strings"
)

type RequiredEnvError struct {
	Missing []string
}

var _ error = &RequiredEnvError{}

func (e *RequiredEnvError) Error() string {
	var msg strings.Builder

	fmt.Fprintln(&msg, "The following environment variables must be set:")
	for _, v := range e.Missing {
		fmt.Fprintf(&msg, " - %s\n", v)
	}
	fmt.Fprintln(&msg, "\nEither:\n - Set those environment variables before running docker agent\n - Run docker agent with --env-from-file\n - Store those secrets using one of the built-in environment variable providers.")

	return msg.String()
}
