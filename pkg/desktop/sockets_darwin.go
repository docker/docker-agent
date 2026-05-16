package desktop

import (
	"os"
	"path/filepath"
)

func getDockerDesktopPaths() (DockerDesktopPaths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return DockerDesktopPaths{}, err
	}

	data := filepath.Join(home, "Library", "Containers", "com.docker.docker", "Data")

	return DockerDesktopPaths{
		BackendSocket: filepath.Join(data, "backend.sock"),
		ProxySocket:   filepath.Join(data, "httpproxy.sock"),
	}, nil
}

func IsWSL() bool {
	return false
}
