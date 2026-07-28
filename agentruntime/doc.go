// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

// Package agentruntime implements the Kaalm Runtime Contract
// (docs/src/runtime/contract.md) as a reusable library, so a custom Go agent
// is a main function and a handler rather than six hundred lines of contract
// plumbing. It is the runtime inside the kaalm-agent-go reference base image
// and the module the starter template imports.
//
// The minimal agent serves the built-in echo handler:
//
//	a, err := agentruntime.New()
//	if err != nil {
//		log.Fatal(err)
//	}
//	log.Fatal(a.Run(ctx, nil))
//
// A real agent passes a Handler and reaches the runtime's capabilities by
// closing over the Agent:
//
//	err = a.Run(ctx, func(ctx context.Context, env agentruntime.Envelope) (agentruntime.Response, error) {
//		var notes []string
//		_, _ = a.Memory.Get("notes/"+env.UserID, &notes)
//		resp, err := a.Gateway.Post(ctx, "/v1/chat/completions", llmRequest(env, notes))
//		...
//	})
//
// The runtime owns everything the contract makes repetitive and error-prone:
// mTLS serving with certificate rotation, per-path gateway identity checks,
// messageId deduplication persisted across hibernation (contract item 7),
// heartbeats with task-mode detection, and the task completion call. This
// exported API is append-only within a minor release series.
package agentruntime
