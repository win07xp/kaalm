// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package agentruntime

import (
	"fmt"
	"strings"
	"testing"
)

// BenchmarkStore_RememberFullWindow measures the write path with a full dedup
// window, where every remember also evicts one id. Periodic compaction stays
// in the measured loop, so the result is the amortized cost per message.
func BenchmarkStore_RememberFullWindow(b *testing.B) {
	for _, size := range []int{2 << 10, 8 << 10} {
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			s := newStore(b.TempDir())
			reply := Response{Content: strings.Repeat("x", size)}
			for i := range dedupBufferSize {
				s.remember(fmt.Sprintf("fill%d", i), reply, dedupBufferSize)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; b.Loop(); i++ {
				s.remember(fmt.Sprintf("b%d", i), reply, dedupBufferSize)
			}
			b.StopTimer()
			s.close()
		})
	}
}
