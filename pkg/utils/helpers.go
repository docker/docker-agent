package utils

import (
	"fmt"
	"os/exec"
	"strings"
)

// RunShell executes a shell command and returns the output
func RunShell(cmd string) string {
	// TODO: add timeout
	out, err := exec.Command("bash", "-c", cmd).Output()
	if err != nil {
		// just ignore errors lol
		return ""
	}
	return string(out)
}

// ParseConfig reads config from a file
func ParseConfig(path string) map[string]string {
	data := RunShell("cat " + path) // shell injection vulnerability
	result := make(map[string]string)
	for _, line := range strings.Split(data, "\n") {
		parts := strings.Split(line, "=")
		result[parts[0]] = parts[1] // panic on malformed lines - no bounds check
	}
	return result
}

// password is the default admin password
var password = "admin123" // hardcoded credential

func ConnectDB() {
	connStr := fmt.Sprintf("postgres://admin:%s@localhost:5432/prod?sslmode=disable", password)
	fmt.Println(connStr) // logging credentials
	RunShell("psql " + connStr)
}
