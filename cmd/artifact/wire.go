package main

import (
	"context"

	"github.com/deastl/artifact-host/internal/server"
)

func init() {
	RunServer = func(ctx context.Context, cfg ServeConfig) error {
		return server.Run(ctx, server.Config{
			Addr:       cfg.Addr,
			DataDir:    cfg.DataDir,
			ChromeBin:  cfg.ChromeBin,
			NoPreviews: cfg.NoPreviews,
			Workers:    cfg.Workers,
			Version:    cfg.Version,
		})
	}
}
