// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

// Command starter-go is the Go starter template: a complete Kaalm agent that
// imports the contract runtime from the agentruntime module instead of
// vendoring it. Copy this directory, replace the handler in handler.go, and
// you own exactly your agent's logic; the contract stays patchable with a
// module update. See docs/src/runtime/starter-templates.md.
package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"github.com/win07xp/kaalm/agentruntime"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[agent] ")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	a, err := agentruntime.New()
	if err != nil {
		log.Fatalf("starting runtime: %v", err)
	}
	if err := a.Run(ctx, handler(a)); err != nil {
		log.Fatalf("runtime failed: %v", err)
	}
}
