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

import "bytes"

// bytesPerToken is the gateway's rule of thumb for text: about four bytes
// per token. Token reservations and stream estimates both use it.
const bytesPerToken = 4

// contentEstimator is the stream estimator the adapters share, each with its
// own member lists. Input is inputEstimate of the body the provider
// received: it leaves out the format's images, audio, and video, which the
// provider bills by dimensions or duration; documents count. It is computed
// only when asked for, since most streams report their input.
// Output adds, per event, the byte length of the values of the format's
// generated-content members over bytesPerToken, at least one token when the
// event held any content; the value of a skipped member (such as log
// probabilities) is passed over uncounted. It scans bytes and decodes no
// JSON, since it runs on every event of every stream.
type contentEstimator struct {
	request []byte
	media   []mediaMember
	output  int64
	// names lists the content members, then the skipped members; the first
	// content of them are content.
	names   [][]byte
	content int
}

// mediaMember names a request member whose string value can be media. A
// value stays out of the input estimate only when its media type is an
// image, audio, or video type (see isMediaType), which the provider bills by
// dimensions or duration; any other value, such as a document, counts. With
// a parent, the member is name inside an object that is the value of a
// member named parent, such as data inside inlineData; a member named name
// anywhere else is text and counts.
type mediaMember struct {
	parent, name []byte
	// typeMember names the sibling member, in the same object, that holds
	// the value's media type, for kind mediaByType.
	typeMember []byte
	kind       mediaKind
}

// mediaKind says where a media member's type is read.
type mediaKind uint8

const (
	// mediaByType: the sibling member typeMember holds the type, before or
	// after the value.
	mediaByType mediaKind = iota
	// mediaDataURL: the value is a data URL, and its type follows "data:".
	mediaDataURL
	// mediaAlways: the member holds media by definition, such as the data
	// of OpenAI's input_audio.
	mediaAlways
)

func newContentEstimator(request []byte, content, skipped [][]byte, media []mediaMember) *contentEstimator {
	return &contentEstimator{
		request: request,
		media:   media,
		names:   append(append(make([][]byte, 0, len(content)+len(skipped)), content...), skipped...),
		content: len(content),
	}
}

func (e *contentEstimator) event(data []byte) {
	n := 0
	s := memberScanner{data: data, names: e.names}
	for {
		name, value, ok := s.next()
		if !ok {
			break
		}
		if name < e.content {
			n += valueLen(value)
		}
		s.skip(value)
	}
	if n > 0 {
		e.output += max(1, int64(n/bytesPerToken))
	}
}

func (e *contentEstimator) estimateInput() int64 { return inputEstimate(e.request, e.media) }

// inputEstimate is the input estimate the token reservation and the stream
// estimate share: request's bytes less its images, audio, and video under
// the media rules (see mediaLen), over bytesPerToken.
func inputEstimate(request []byte, media []mediaMember) int64 {
	return int64((len(request) - mediaLen(request, media)) / bytesPerToken)
}

func (e *contentEstimator) estimateOutput() int64 { return e.output }

// mediaLen sums the lengths of the string values of data's media members
// whose media type is an image, audio, or video type, counted as
// jsonMembersLen counts a string. It walks data once, jumping each string,
// and keeps a frame per open object or array. A frame holds the member name
// that opened it, so a parent rule matches a string member of the innermost
// open object; and, for a mediaByType rule, the bytes of the object's values
// and the verdict of its type member, decided when the object closes, so
// the type member may come before or after the value and no object is
// walked twice. Every copy of a value in an object is summed, and the last
// copy of the type member decides, as parsers that keep the last copy do; a
// copy that is not a string makes the type unknown, and the values count. A
// string cut off before it ends ends the walk, and the objects still open
// leave out nothing.
func mediaLen(data []byte, media []mediaMember) int {
	if len(media) == 0 {
		return 0
	}
	total := 0
	// A request nests a handful of levels deep; the first sixteen frames
	// need no allocation.
	var stack [16]mediaFrame
	frames := stack[:0]
	// key is the member name whose value comes next, nil when none does.
	var key []byte
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '"':
			end, ok := stringEnd(data, i)
			if !ok {
				return total
			}
			next := skipSpace(data, end+1)
			if next < len(data) && data[next] == ':' {
				key = data[i+1 : end]
				i = next
				continue
			}
			if key != nil && len(frames) > 0 {
				total += mediaString(media, &frames[len(frames)-1], key, data[i+1:end])
			}
			key = nil
			i = end
		case '{':
			frames = append(frames, mediaFrame{opener: noteNonString(media, frames, key)})
			key = nil
		case '[':
			noteNonString(media, frames, key)
			frames = append(frames, mediaFrame{})
			key = nil
		case '}', ']':
			if n := len(frames); n > 0 {
				if f := frames[n-1]; f.media {
					total += f.pending
				}
				frames = frames[:n-1]
			}
			key = nil
		case ' ', '\t', '\n', '\r':
		default:
			noteNonString(media, frames, key)
			key = nil
		}
	}
	return total
}

// mediaFrame is one open object or array in mediaLen's walk. opener is the
// name of the member whose value it is (nil for an array, a top-level
// value, or an array item). pending holds the bytes of the object's
// mediaByType values, left out when the object closes if media, the
// verdict of the last type member seen, is true.
type mediaFrame struct {
	opener  []byte
	pending int
	media   bool
}

// mediaString applies the rules to str, the string value of the member
// named key in the object of frame f, and returns the bytes left out at
// once. A mediaByType value goes to f's pending instead, and a type member
// sets f's verdict.
func mediaString(media []mediaMember, f *mediaFrame, key, str []byte) int {
	for _, m := range media {
		if !parentMatches(m, f.opener) {
			continue
		}
		switch {
		case bytes.Equal(key, m.name):
			switch m.kind {
			case mediaAlways:
				return len(str)
			case mediaDataURL:
				if t, ok := bytes.CutPrefix(str, []byte("data:")); ok && isMediaType(t) {
					return len(str)
				}
			case mediaByType:
				f.pending += len(str)
				return 0
			}
		case m.kind == mediaByType && bytes.Equal(key, m.typeMember):
			f.media = isMediaType(str)
		}
	}
	return 0
}

// noteNonString handles a value of the member named key that is not a
// string, in the innermost open object: a type member with such a value
// makes the object's type unknown. It returns key, the opener of an object
// value.
func noteNonString(media []mediaMember, frames []mediaFrame, key []byte) []byte {
	if key == nil || len(frames) == 0 {
		return key
	}
	f := &frames[len(frames)-1]
	for _, m := range media {
		if m.kind == mediaByType && bytes.Equal(key, m.typeMember) && parentMatches(m, f.opener) {
			f.media = false
		}
	}
	return key
}

// parentMatches reports whether rule m applies to a member of an object
// that is the value of a member named opener.
func parentMatches(m mediaMember, opener []byte) bool {
	return m.parent == nil || (opener != nil && bytes.Equal(opener, m.parent))
}

// isMediaType reports whether the media type t, as written between its
// quotes, is an image, audio, or video type: it starts with "image",
// "audio", or "video" under ASCII case folding, then a slash. The slash may
// be written `\/`, as encoders such as PHP's json_encode do by default; any
// other escape is compared raw, so the value counts. Parameters after the
// type are ignored.
func isMediaType(t []byte) bool {
	if len(t) < 6 {
		return false
	}
	p := t[:5]
	if !bytes.EqualFold(p, []byte("image")) && !bytes.EqualFold(p, []byte("audio")) && !bytes.EqualFold(p, []byte("video")) {
		return false
	}
	return t[5] == '/' || bytes.HasPrefix(t[5:], []byte(`\/`))
}

// jsonMembersLen sums the value lengths of every member of data named one of
// names, at any depth. A string value counts the bytes between its quotes as
// written (escapes count raw); an object or array counts the bytes from its
// opening bracket to its matching close, and a member inside it is not
// counted again; any other value, and a value cut off before it ends,
// counts zero.
func jsonMembersLen(data []byte, names [][]byte) int {
	total := 0
	s := memberScanner{data: data, names: names}
	for {
		_, value, ok := s.next()
		if !ok {
			return total
		}
		total += valueLen(value)
		s.skip(value)
	}
}

// valueLen is the length jsonMembersLen counts for a value from
// memberScanner.next: a string's bytes between its quotes, an object's or
// array's bytes from bracket to bracket.
func valueLen(value []byte) int {
	if len(value) >= 2 && value[0] == '"' {
		return len(value) - 2
	}
	return len(value)
}

// memberScanner finds the members of data named one of names, at any
// depth, in order. It visits each colon: one that follows an unescaped quote
// (past any whitespace) ends a member name, which it compares with names. A
// colon inside a string follows no such quote, since a quote inside a
// string is escaped, so neither a value equal to a name nor a name inside a
// string value matches.
type memberScanner struct {
	data  []byte
	names [][]byte
	i     int
}

// next returns the index in names of the next matching member and its
// value: a string with its quotes, or an object or array from bracket to
// bracket. The value is nil for any other value; a value cut off before it
// ends is nil and ends the scan. The scan goes on inside the value unless
// skip is called with it.
func (s *memberScanner) next() (name int, value []byte, ok bool) {
	data := s.data
	for s.i < len(data) {
		c := bytes.IndexByte(data[s.i:], ':')
		if c < 0 {
			break
		}
		colon := s.i + c
		s.i = colon + 1
		closing := colon - 1
		for closing >= 0 && isSpace(data[closing]) {
			closing--
		}
		if closing < 0 || data[closing] != '"' || escaped(data, closing) {
			continue
		}
		name = memberIndex(data, closing, s.names)
		if name < 0 {
			continue
		}
		start := skipSpace(data, s.i)
		end, complete := jsonValueEnd(data, start)
		if !complete {
			s.i = max(s.i, end)
			return name, nil, true
		}
		s.i = start
		return name, data[start:end], true
	}
	s.i = len(data)
	return 0, nil, false
}

// skip moves the scan past value, the last value next returned: next
// leaves the scan at the value's first byte.
func (s *memberScanner) skip(value []byte) { s.i += len(value) }

// memberIndex returns the index in names of the string whose closing quote
// is data[closing], or -1 when it is none of them. The string opens at the
// last quote before it; when that quote is escaped, the string holds a
// quote, which no name does.
func memberIndex(data []byte, closing int, names [][]byte) int {
	open := bytes.LastIndexByte(data[:closing], '"')
	if open < 0 || escaped(data, open) {
		return -1
	}
	name := data[open+1 : closing]
	for i, n := range names {
		if len(n) == len(name) && bytes.Equal(name, n) {
			return i
		}
	}
	return -1
}

// escaped reports whether the byte at data[i] follows an odd run of
// backslashes.
func escaped(data []byte, i int) bool {
	n := 0
	for i-1-n >= 0 && data[i-1-n] == '\\' {
		n++
	}
	return n%2 == 1
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

func skipSpace(data []byte, i int) int {
	for i < len(data) && isSpace(data[i]) {
		i++
	}
	return i
}

// jsonValueEnd returns the index after the value at data[i] and whether it
// is a complete string, object, or array. Any other value returns i, and a
// value that ends before it closes returns len(data), both incomplete.
func jsonValueEnd(data []byte, i int) (end int, complete bool) {
	if i >= len(data) {
		return i, false
	}
	switch data[i] {
	case '"':
		end, ok := stringEnd(data, i)
		if !ok {
			return len(data), false
		}
		return end + 1, true
	case '{', '[':
		depth := 0
		for j := i; j < len(data); j++ {
			switch data[j] {
			case '"':
				end, ok := stringEnd(data, j)
				if !ok {
					return len(data), false
				}
				j = end
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1, true
				}
			}
		}
		return len(data), false
	}
	return i, false
}

// stringEnd returns the index of the quote closing the string that opens at
// data[i]: the first quote after it not escaped by an odd run of
// backslashes.
func stringEnd(data []byte, i int) (int, bool) {
	for j := i + 1; ; {
		k := bytes.IndexByte(data[j:], '"')
		if k < 0 {
			return 0, false
		}
		p := j + k
		backslashes := 0
		for p-1-backslashes > i && data[p-1-backslashes] == '\\' {
			backslashes++
		}
		if backslashes%2 == 0 {
			return p, true
		}
		j = p + 1
	}
}
