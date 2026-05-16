package useragent

import (
	"fmt"
	"runtime"

	"github.com/docker/docker-agent/pkg/version"
)

var Header = fmt.Sprintf("Cagent/%s (%s; %s)", version.Version, runtime.GOOS, runtime.GOARCH)
