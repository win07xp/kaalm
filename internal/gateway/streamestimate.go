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
// own content members. Input is the body the provider received over
// bytesPerToken. Output adds, per event, the byte length of the values of
// the format's generated-content members over bytesPerToken, at least one
// token when the event held any content. It scans bytes and decodes no JSON,
// since it runs on every event of every stream.
type contentEstimator struct {
	input, output int64
	members       [][]byte
}

func newContentEstimator(request []byte, members [][]byte) *contentEstimator {
	return &contentEstimator{input: int64(len(request) / bytesPerToken), members: members}
}

func (e *contentEstimator) event(data []byte) {
	if n := jsonMembersLen(data, e.members); n > 0 {
		e.output += max(1, int64(n/bytesPerToken))
	}
}

func (e *contentEstimator) estimate() (input, output int64) { return e.input, e.output }

// jsonMembersLen sums the value lengths of every member of data named one of
// names, at any depth. A string value counts the bytes between its quotes as
// written (escapes count raw); an object or array counts the bytes from its
// opening bracket to its matching close, and a member inside it is not
// counted again; any other value, and a value cut off before it ends,
// counts zero. It visits each colon: one that follows an unescaped quote
// (past any whitespace) ends a member name, which it compares with names. A
// colon inside a string follows no such quote, since a quote inside a
// string is escaped, so neither a value equal to a name nor a name inside a
// string value matches.
func jsonMembersLen(data []byte, names [][]byte) int {
	total := 0
	for i := 0; i < len(data); {
		c := bytes.IndexByte(data[i:], ':')
		if c < 0 {
			break
		}
		colon := i + c
		i = colon + 1
		closing := colon - 1
		for closing >= 0 && isSpace(data[closing]) {
			closing--
		}
		if closing < 0 || data[closing] != '"' || escaped(data, closing) || !endsMember(data, closing, names) {
			continue
		}
		n, next := jsonValueLen(data, skipSpace(data, i))
		total += n
		i = max(i, next)
	}
	return total
}

// endsMember reports whether the quote at data[closing] closes a string
// that is one of names. The string opens at the last quote before it; when
// that quote is escaped, the string holds a quote, which no name does.
func endsMember(data []byte, closing int, names [][]byte) bool {
	open := bytes.LastIndexByte(data[:closing], '"')
	if open < 0 || escaped(data, open) {
		return false
	}
	name := data[open+1 : closing]
	for _, n := range names {
		if len(n) == len(name) && bytes.Equal(name, n) {
			return true
		}
	}
	return false
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

// jsonValueLen measures the value at data[i] for jsonMembersLen and returns
// its length and the index after it. A value that is not a string, object,
// or array, or that ends before it closes, measures zero.
func jsonValueLen(data []byte, i int) (n, next int) {
	if i >= len(data) {
		return 0, i
	}
	switch data[i] {
	case '"':
		end, ok := stringEnd(data, i)
		if !ok {
			return 0, len(data)
		}
		return end - i - 1, end + 1
	case '{', '[':
		depth := 0
		for j := i; j < len(data); j++ {
			switch data[j] {
			case '"':
				end, ok := stringEnd(data, j)
				if !ok {
					return 0, len(data)
				}
				j = end
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j - i + 1, j + 1
				}
			}
		}
		return 0, len(data)
	}
	return 0, i
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
