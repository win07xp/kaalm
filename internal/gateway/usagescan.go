/*
Copyright 2026 The Kaalm Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gateway

import (
	"bytes"
	"encoding/json"
)

// The relay reads usage from a JSON answer while it copies the answer to
// the agent, so the gateway never holds the whole answer. usageScanner is a
// push-mode JSON scanner: it follows the document's structure byte by byte
// and keeps only the values at a few fixed paths. Its memory is constant
// whatever the answer's size: the bulk of an answer is string content,
// which it skips without copying.

// jsonPath names a value by the member names from the top-level object
// down; "*" matches every element of an array.
type jsonPath []string

const (
	// usageCaptureLimit bounds one captured value. A usage member is well
	// under a kilobyte; a larger value is dropped, and the answer counts as
	// carrying no usage.
	usageCaptureLimit = 64 << 10
	// usageKeyLimit bounds a member name the scanner compares. A longer name
	// matches no path.
	usageKeyLimit = 64
	// maxScanDepth is the longest path the scanner supports. Deeper
	// containers are only counted.
	maxScanDepth = 4
)

// scanFrame is one open container at a depth the paths reach.
type scanFrame struct {
	array bool
	// expectKey: in an object, the next string is a member name.
	expectKey bool
	// mask holds the paths whose names match the members leading here.
	mask uint32
}

// The kinds of a captured value.
const (
	captureContainer = iota
	captureString
	captureScalar
)

// usageScanner calls onValue with the raw bytes of each value found at one
// of its paths, in document order. Malformed JSON never fails a Write: the
// scanner stops matching instead.
type usageScanner struct {
	paths   []jsonPath
	onValue func(path int, value []byte)
	all     uint32

	frames [maxScanDepth]scanFrame
	depth  int // open containers, including those deeper than frames
	dead   bool
	// rootDone: the top-level value ended; another one is malformed.
	rootDone bool

	inString, escape, stringIsKey bool
	inScalar                      bool
	key                           [usageKeyLimit]byte
	keyLen                        int
	keyOver                       bool
	// pending is the mask for the value after the member name just read.
	pending uint32

	capturing bool
	capKind   int
	capPath   int
	capDepth  int
	capStart  int // where the capture starts in the current Write
	capOver   bool
	capBuf    []byte
}

func newUsageScanner(onValue func(path int, value []byte), paths ...jsonPath) *usageScanner {
	if len(paths) > 32 {
		panic("usageScanner: more than 32 paths")
	}
	s := &usageScanner{paths: paths, onValue: onValue}
	for i, p := range paths {
		if len(p) == 0 || len(p) > maxScanDepth {
			panic("usageScanner: path length out of range")
		}
		s.all |= 1 << i
	}
	return s
}

// Write scans p. It always consumes all of p.
func (s *usageScanner) Write(p []byte) (int, error) {
	if s.dead {
		return len(p), nil
	}
	s.capStart = 0
	for i := 0; i < len(p) && !s.dead; {
		if s.inString {
			i = s.stringBytes(p, i)
			continue
		}
		c := p[i]
		if s.inScalar {
			if !scalarEnds(c) {
				i++
				continue
			}
			s.endScalar(p, i)
		}
		switch c {
		case ' ', '\t', '\n', '\r', ':':
		case '"':
			s.startString(i)
		case ',':
			if f := s.top(); f != nil && !f.array {
				f.expectKey = true
			}
		case '{', '[':
			s.open(i, c == '[')
		case '}', ']':
			s.close(p, i)
		default:
			s.startValue(i, captureScalar)
			s.inScalar = true
		}
		i++
	}
	if s.capturing {
		s.appendCapture(p[s.capStart:])
	}
	return len(p), nil
}

// scalarEnds reports whether c ends a number, true, false, or null.
func scalarEnds(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', ',', ':', '}', ']', '"', '{', '[':
		return true
	}
	return false
}

// top returns the innermost open container's frame, or nil when none is
// open or it lies deeper than the paths reach.
func (s *usageScanner) top() *scanFrame {
	if s.depth == 0 || s.depth > maxScanDepth {
		return nil
	}
	return &s.frames[s.depth-1]
}

// match narrows mask to the paths whose segment at the current depth is
// name.
func (s *usageScanner) match(mask uint32, name []byte) uint32 {
	var out uint32
	for i, p := range s.paths {
		if mask&(1<<i) != 0 && len(p) >= s.depth && p[s.depth-1] == string(name) {
			out |= 1 << i
		}
	}
	return out
}

// matchElement narrows mask to the paths that take every element of the
// array at the current depth.
func (s *usageScanner) matchElement(mask uint32) uint32 {
	var out uint32
	for i, p := range s.paths {
		if mask&(1<<i) != 0 && len(p) >= s.depth && p[s.depth-1] == "*" {
			out |= 1 << i
		}
	}
	return out
}

// startValue handles the first byte of a value of the given kind, at index
// i of the current Write, and returns the mask a container value opens
// with.
func (s *usageScanner) startValue(i, kind int) uint32 {
	if s.depth == 0 {
		if s.rootDone {
			s.dead = true
			return 0
		}
		return s.all
	}
	var mask uint32
	if f := s.top(); f != nil {
		if f.array {
			mask = s.matchElement(f.mask)
		} else {
			mask = s.pending
			s.pending = 0
		}
	}
	if s.capturing || mask == 0 {
		return mask
	}
	for idx, path := range s.paths {
		if mask&(1<<idx) != 0 && len(path) == s.depth {
			s.capturing, s.capKind, s.capPath, s.capDepth = true, kind, idx, s.depth
			s.capStart, s.capOver, s.capBuf = i, false, s.capBuf[:0]
			return 0
		}
	}
	return mask
}

func (s *usageScanner) startString(i int) {
	s.inString, s.escape, s.stringIsKey = true, false, false
	if f := s.top(); f != nil && !f.array && f.expectKey {
		f.expectKey = false
		// A name under no path is skipped like any string.
		s.stringIsKey, s.keyLen, s.keyOver = true, 0, f.mask == 0
		return
	}
	s.startValue(i, captureString)
}

// stringBytes consumes string content from p[i:] and returns the next index.
func (s *usageScanner) stringBytes(p []byte, i int) int {
	if s.escape {
		s.escape = false
		s.keyAppend(p[i : i+1])
		return i + 1
	}
	j := bytes.IndexAny(p[i:], "\"\\")
	if j < 0 {
		s.keyAppend(p[i:])
		return len(p)
	}
	k := i + j
	if p[k] == '\\' {
		s.keyAppend(p[i : k+1])
		s.escape = true
		return k + 1
	}
	s.keyAppend(p[i:k])
	s.inString = false
	switch {
	case s.stringIsKey:
		s.pending = 0
		if !s.keyOver {
			s.pending = s.match(s.top().mask, s.key[:s.keyLen])
		}
	case s.capturing && s.capKind == captureString && s.depth == s.capDepth:
		s.endCapture(p, k+1)
	case s.depth == 0:
		s.rootDone = true
	}
	return k + 1
}

func (s *usageScanner) keyAppend(b []byte) {
	if !s.stringIsKey || s.keyOver {
		return
	}
	if s.keyLen+len(b) > usageKeyLimit {
		s.keyOver = true
		return
	}
	s.keyLen += copy(s.key[s.keyLen:], b)
}

func (s *usageScanner) endScalar(p []byte, i int) {
	s.inScalar = false
	switch {
	case s.capturing && s.capKind == captureScalar && s.depth == s.capDepth:
		s.endCapture(p, i)
	case s.depth == 0:
		s.rootDone = true
	}
}

func (s *usageScanner) open(i int, array bool) {
	mask := s.startValue(i, captureContainer)
	if s.depth < maxScanDepth {
		s.frames[s.depth] = scanFrame{array: array, expectKey: !array, mask: mask}
	}
	s.depth++
}

func (s *usageScanner) close(p []byte, i int) {
	if s.depth == 0 {
		s.dead = true
		return
	}
	s.depth--
	switch {
	case s.capturing && s.capKind == captureContainer && s.depth == s.capDepth:
		s.endCapture(p, i+1)
	case s.depth == 0:
		s.rootDone = true
	}
}

func (s *usageScanner) appendCapture(b []byte) {
	if s.capOver {
		return
	}
	if len(s.capBuf)+len(b) > usageCaptureLimit {
		s.capOver, s.capBuf = true, s.capBuf[:0]
		return
	}
	s.capBuf = append(s.capBuf, b...)
}

// endCapture ends the captured value at p[:end] and reports it, unless it
// passed the capture limit.
func (s *usageScanner) endCapture(p []byte, end int) {
	s.appendCapture(p[s.capStart:end])
	s.capturing = false
	if !s.capOver {
		s.onValue(s.capPath, s.capBuf)
	}
	s.capBuf = s.capBuf[:0]
}

// usageReader is an adapter's usage reader for a JSON answer: the relay
// writes every byte of the answer to it, then reads the result. The usage
// members at the format's paths fold into usage as they complete.
type usageReader struct {
	scan  *usageScanner
	usage Usage
}

// Write scans p for usage. It never fails.
func (r *usageReader) Write(p []byte) (int, error) { return r.scan.Write(p) }

// result returns the usage read so far; ok is false when the answer carried
// none.
func (r *usageReader) result() (Usage, bool) {
	if r.usage.isZero() {
		return Usage{}, false
	}
	return r.usage, true
}

// newAnthropicUsageReader reads the top-level usage member. A repeated
// member replaces the last, as a decode would.
func newAnthropicUsageReader() *usageReader {
	r := &usageReader{}
	r.scan = newUsageScanner(func(_ int, v []byte) {
		var u struct {
			InputTokens   int64           `json:"input_tokens"`
			OutputTokens  int64           `json:"output_tokens"`
			ServerToolUse json.RawMessage `json:"server_tool_use"`
		}
		if json.Unmarshal(v, &u) != nil {
			return
		}
		r.usage = Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
			ServerTools: serverToolCounts(u.ServerToolUse)}
	}, jsonPath{"usage"})
	return r
}

// newOpenAIUsageReader reads the top-level usage member.
func newOpenAIUsageReader() *usageReader {
	r := &usageReader{}
	r.scan = newUsageScanner(func(_ int, v []byte) {
		var u struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		}
		if json.Unmarshal(v, &u) != nil {
			return
		}
		r.usage = Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens}
	}, jsonPath{"usage"})
	return r
}

// Paths of the Vertex usage reader.
const (
	vertexPathUsage = iota
	vertexPathQueries
)

// newVertexUsageReader reads usageMetadata, with thinking tokens billed as
// output, and counts the grounding queries of every candidate as the
// google_search server tool.
func newVertexUsageReader() *usageReader {
	r := &usageReader{}
	r.scan = newUsageScanner(func(path int, v []byte) {
		switch path {
		case vertexPathUsage:
			var u struct {
				PromptTokenCount     int64 `json:"promptTokenCount"`
				CandidatesTokenCount int64 `json:"candidatesTokenCount"`
				ThoughtsTokenCount   int64 `json:"thoughtsTokenCount"`
			}
			if json.Unmarshal(v, &u) != nil {
				return
			}
			r.usage.InputTokens = u.PromptTokenCount
			r.usage.OutputTokens = u.CandidatesTokenCount + u.ThoughtsTokenCount
		case vertexPathQueries:
			var queries []json.RawMessage
			if json.Unmarshal(v, &queries) != nil || len(queries) == 0 {
				return
			}
			if r.usage.ServerTools == nil {
				r.usage.ServerTools = map[string]int64{}
			}
			r.usage.ServerTools["google_search"] += int64(len(queries))
		}
	}, jsonPath{"usageMetadata"}, jsonPath{"candidates", "*", "groundingMetadata", "webSearchQueries"})
	return r
}
