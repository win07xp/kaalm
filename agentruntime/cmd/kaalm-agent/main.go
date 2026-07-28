// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

// Command kaalm-agent is the reference agent inside the kaalm-agent-go base
// image: the full runtime contract serving the built-in default handler.
// Apply an Agent with this image and no handler of any kind, and it comes up,
// heartbeats, hibernates, wakes, and echoes (docs/src/runtime/base-images.md).
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
	if err := a.Run(ctx, nil); err != nil {
		log.Fatalf("runtime failed: %v", err)
	}
}
