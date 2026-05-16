package desktop

import (
	"errors"
	"os"
	"path/filepath"
)

func getDockerDesktopPaths() (DockerDesktopPaths, error) {
	_, err := os.Stat("/run/host-services/backend.sock")
	if err == nil {
		// Inside LinuxKit
		return DockerDesktopPaths{
			BackendSocket: "/run/host-services/backend.sock",
			ProxySocket:   "/run/host-services/httpproxy.sock",
		}, nil
	}

	if !errors.Is(err, os.ErrNotExist) {
		return DockerDesktopPaths{}, err
	}

	if IsWSL() {
		return DockerDesktopPaths{
			BackendSocket: "/mnt/wsl/docker-desktop/shared-sockets/host-services/backend.sock",
			ProxySocket:   "/mnt/wsl/docker-desktop/shared-sockets/host-services/http-proxy.sock",
		}, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return DockerDesktopPaths{}, err
	}

	// On Linux
	return DockerDesktopPaths{
		BackendSocket: filepath.Join(home, ".docker", "desktop", "backend.sock"),
		ProxySocket:   filepath.Join(home, ".docker", "desktop", "httpproxy.sock"),
	}, nil
}

func IsWSL() bool {
	if _, err := os.Stat("/mnt/wsl/docker-desktop/shared-sockets/host-services/backend.sock"); err == nil {
		return true
	}
	return false
}
