package main

import "context"

// ServeConfig carries configuration passed to the embedded HTTP server runtime.
type ServeConfig struct {
	Addr       string
	DataDir    string
	ChromeBin  string
	NoPreviews bool
	Workers    int
	Version    string
}

// RunServer is wired by the binary entrypoint to start the artifact-host daemon.
var RunServer func(ctx context.Context, cfg ServeConfig) error
