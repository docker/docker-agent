package desktop

import "sync"

type DockerDesktopPaths struct {
	BackendSocket string
	ProxySocket   string
}

var Paths = sync.OnceValue(func() DockerDesktopPaths {
	desktopPaths, err := getDockerDesktopPaths()
	if err != nil {
		panic(err)
	}

	return desktopPaths
})
