package desktop

import (
	"context"
)

type DockerHubInfo struct {
	Username string `json:"id"`
	Email    string `json:"email,omitempty"`
}

func GetToken(ctx context.Context) string {
	var token string
	_ = ClientBackend.Get(ctx, "/registry/token", &token)
	return token
}

func GetUserInfo(ctx context.Context) DockerHubInfo {
	var info DockerHubInfo
	_ = ClientBackend.Get(ctx, "/registry/info", &info)
	return info
}
