package desktop

import (
	"context"
	"time"
)

func IsDockerDesktopRunning(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, time.Second*3)
	defer cancel()
	err := ClientBackend.Get(ctx, "/ping", nil)
	return err == nil
}
