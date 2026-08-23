package adapter

import (
	"context"

	"vps-tool/agent/internal/model"
)

type Warp interface {
	Status(context.Context) (model.WarpState, error)
	On(context.Context) error
	Off(context.Context) error
}

type XUI interface {
	Status(context.Context, string) (model.XUIState, error)
	Restart(context.Context, string) error
}

type IPChecker interface {
	IPv4(context.Context) (string, error)
	IPv6(context.Context) (string, error)
}
