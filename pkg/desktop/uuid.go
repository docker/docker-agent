package desktop

import (
	"context"

	"github.com/kofalt/go-memoize"
	"github.com/patrickmn/go-cache"
)

var uuidMmemoizer = memoize.NewMemoizer(cache.NoExpiration, cache.NoExpiration)

func GetUUID(ctx context.Context) string {
	uuid, _, _ := uuidMmemoizer.Memoize("desktopUUID", func() (any, error) {
		var uuid string
		_ = ClientBackend.Get(ctx, "/uuid", &uuid)
		return uuid, nil
	})
	return uuid.(string)
}
