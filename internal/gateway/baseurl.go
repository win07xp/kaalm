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
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ParseBaseURL checks that value is a base URL that callers can append path
// segments to: an http or https URL with a host, an optional port in
// 1-65535, and an optional path, with no user info, query, or fragment. A
// query or fragment (even a bare ? or #) would swallow the appended path,
// and user info would leak into logs or be dropped by the client. label
// starts every error message. A parse failure reports only the inner cause,
// and the other errors name the value only through hideUserInfo, so an error
// never carries a password or token.
func ParseBaseURL(label, value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, fmt.Errorf("%s: %w", label, ue.Err)
		}
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, fmt.Errorf("%s %s: need http(s)://host[:port][/path]", label, hideUserInfo(u))
	}
	// u.Fragment is empty for a bare #, which still breaks the appended path.
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(value, "#") {
		return nil, fmt.Errorf("%s %s: need http(s)://host[:port][/path] with no user info, query, or fragment", label, hideUserInfo(u))
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("%s %s: port %s is outside 1-65535", label, hideUserInfo(u), p)
		}
	}
	return u, nil
}

// hideUserInfo renders u, quoted, for an error message, with any user info
// replaced by "redacted". url.URL.Redacted masks only a password, and a user
// name with no password is often a bearer token. Without the // after the
// scheme, or without a scheme, url.Parse leaves user info in the opaque part
// or the path rather than in User, so a value with an @ anywhere else is
// left out entirely.
func hideUserInfo(u *url.URL) string {
	if u.User != nil {
		c := *u
		c.User = url.User("redacted")
		return strconv.Quote(c.String())
	}
	s := u.String()
	if strings.Contains(s, "@") {
		return "(value hidden: it may hold user info)"
	}
	return strconv.Quote(s)
}
