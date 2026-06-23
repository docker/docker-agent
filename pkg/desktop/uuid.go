package desktop

import (
	"context"

	"github.com/docker/docker-agent/pkg/memoize"
)

var uuidMemoizer = memoize.New[string](memoize.NoExpiration)

func GetUUID(ctx context.Context) string {
	uuid, _ := uuidMemoizer.Memoize("desktopUUID", func() (string, error) {
		var uuid string
		_ = ClientBackend.Get(ctx, "/uuid", &uuid)
		return uuid, nil
	})
	return uuid
}
