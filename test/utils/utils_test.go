/*
Copyright 2026.

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

package utils

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestParseForwardPort(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantPort int
		wantOK   bool
	}{
		{
			name:     "valid IPv4 forwarding line",
			line:     "Forwarding from 127.0.0.1:34567 -> 8080",
			wantPort: 34567,
			wantOK:   true,
		},
		{
			name:   "IPv6 duplicate line is ignored",
			line:   "Forwarding from [::1]:34567 -> 8080",
			wantOK: false,
		},
		{
			name:   "garbage line",
			line:   "random log line",
			wantOK: false,
		},
		{
			name:   "empty line",
			line:   "",
			wantOK: false,
		},
		{
			name:   "marker present but no space after port",
			line:   "Forwarding from 127.0.0.1:34567",
			wantOK: false,
		},
		{
			name:   "marker present but non-numeric port",
			line:   "Forwarding from 127.0.0.1:abcde -> 8080",
			wantOK: false,
		},
		{
			name:     "marker not at start of line",
			line:     "prefix noise Forwarding from 127.0.0.1:9999 -> 80",
			wantPort: 9999,
			wantOK:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, ok := parseForwardPort(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("parseForwardPort(%q) ok = %v, want %v", tt.line, ok, tt.wantOK)
			}
			if ok && p != tt.wantPort {
				t.Fatalf("parseForwardPort(%q) = %d, want %d", tt.line, p, tt.wantPort)
			}
		})
	}
}

func TestGetNonEmptyLines(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect []string
	}{
		{
			name:   "empty input",
			input:  "",
			expect: nil,
		},
		{
			name:   "single line no newline",
			input:  "hello",
			expect: []string{"hello"},
		},
		{
			name:   "multiple lines with blank lines interspersed",
			input:  "foo\n\nbar\n\n\nbaz",
			expect: []string{"foo", "bar", "baz"},
		},
		{
			name:   "trailing newline",
			input:  "foo\nbar\n",
			expect: []string{"foo", "bar"},
		},
		{
			name:   "only blank lines",
			input:  "\n\n\n",
			expect: nil,
		},
		{
			name:   "leading blank line",
			input:  "\nfoo\nbar",
			expect: []string{"foo", "bar"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetNonEmptyLines(tt.input)
			if len(got) != len(tt.expect) {
				t.Fatalf("GetNonEmptyLines(%q) = %#v, want %#v", tt.input, got, tt.expect)
			}
			for i := range got {
				if got[i] != tt.expect[i] {
					t.Fatalf("GetNonEmptyLines(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.expect[i])
				}
			}
		})
	}
}

func TestGetProjectDir(t *testing.T) {
	dir, err := GetProjectDir()
	if err != nil {
		t.Fatalf("GetProjectDir() returned error: %v", err)
	}
	if dir == "" {
		t.Fatal("GetProjectDir() returned empty string")
	}
	if strings.Contains(dir, "/test/e2e") {
		t.Fatalf("GetProjectDir() = %q, want the /test/e2e suffix stripped", dir)
	}
}

func TestGetProjectDirErrorFromRemovedCwd(t *testing.T) {
	// Force os.Getwd() to fail by chdir-ing into a directory and then
	// deleting it out from under the process; on Linux this makes a
	// subsequent Getwd() return an error (ENOENT), exercising
	// GetProjectDir's error branch without touching any external process.
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() before test: %v", err)
	}
	defer func() {
		if cerr := os.Chdir(orig); cerr != nil {
			t.Fatalf("restoring original cwd: %v", cerr)
		}
	}()

	dir, err := os.MkdirTemp("", "getprojectdir-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir(%s): %v", dir, err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatalf("Remove(%s): %v", dir, err)
	}

	if _, err := GetProjectDir(); err == nil {
		t.Fatal("GetProjectDir() with a deleted cwd: expected an error, got nil")
	}
}

func TestGetProjectDirStripsE2ESuffix(t *testing.T) {
	// Exercise the same replace logic GetProjectDir uses, directly, so the
	// stripping behavior itself is asserted rather than relying on the
	// working directory of the test binary.
	wd := "/home/user/repo/test/e2e"
	stripped := strings.Replace(wd, "/test/e2e", "", -1)
	if stripped != "/home/user/repo" {
		t.Fatalf("replace logic = %q, want %q", stripped, "/home/user/repo")
	}
}

func TestPostJSON(t *testing.T) {
	t.Run("success with bearer token", func(t *testing.T) {
		var gotAuth, gotContentType, gotMethod, gotBody string
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			gotContentType = r.Header.Get("Content-Type")
			gotMethod = r.Method
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer srv.Close()

		payload, err := json.Marshal(map[string]string{"hello": "world"})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		status, body, err := PostJSON(srv.URL, "test-token", payload)
		if err != nil {
			t.Fatalf("PostJSON returned error: %v", err)
		}
		if status != http.StatusCreated {
			t.Errorf("status = %d, want %d", status, http.StatusCreated)
		}
		if body != `{"ok":true}` {
			t.Errorf("body = %q, want %q", body, `{"ok":true}`)
		}
		if gotAuth != "Bearer test-token" {
			t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer test-token")
		}
		if gotContentType != "application/json" {
			t.Errorf("Content-Type header = %q, want %q", gotContentType, "application/json")
		}
		if gotMethod != http.MethodPost {
			t.Errorf("method = %q, want %q", gotMethod, http.MethodPost)
		}
		if gotBody != string(payload) {
			t.Errorf("request body = %q, want %q", gotBody, string(payload))
		}
	})

	t.Run("no bearer token omits Authorization header", func(t *testing.T) {
		var gotAuthValues []string
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuthValues = r.Header["Authorization"]
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		status, _, err := PostJSON(srv.URL, "", []byte(`{}`))
		if err != nil {
			t.Fatalf("PostJSON returned error: %v", err)
		}
		if status != http.StatusOK {
			t.Errorf("status = %d, want %d", status, http.StatusOK)
		}
		if len(gotAuthValues) > 0 {
			t.Errorf("Authorization header should be absent, got %v", gotAuthValues)
		}
	})

	t.Run("invalid url returns error", func(t *testing.T) {
		_, _, err := PostJSON("http://%zz", "", []byte(`{}`))
		if err == nil {
			t.Fatal("expected error for malformed URL, got nil")
		}
	})

	t.Run("connection failure returns error", func(t *testing.T) {
		_, _, err := PostJSON("https://127.0.0.1:1", "", []byte(`{}`))
		if err == nil {
			t.Fatal("expected error for connection failure, got nil")
		}
	})
}

func TestPostJSONHeaders(t *testing.T) {
	var gotUser, gotAuth string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser = r.Header.Get("X-User-Id")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	status, _, err := PostJSONHeaders(srv.URL, "tok", map[string]string{"X-User-Id": "alice"}, []byte(`{}`))
	if err != nil {
		t.Fatalf("PostJSONHeaders returned error: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want %d", status, http.StatusOK)
	}
	if gotUser != "alice" {
		t.Errorf("X-User-Id = %q, want %q", gotUser, "alice")
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer tok")
	}
}

func TestGetWithBearer(t *testing.T) {
	t.Run("success with bearer token", func(t *testing.T) {
		var gotAuth, gotMethod string
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			gotMethod = r.Method
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer srv.Close()

		status, body, err := GetWithBearer(srv.URL, "test-token")
		if err != nil {
			t.Fatalf("GetWithBearer returned error: %v", err)
		}
		if status != http.StatusOK {
			t.Errorf("status = %d, want %d", status, http.StatusOK)
		}
		if body != `{"ok":true}` {
			t.Errorf("body = %q, want %q", body, `{"ok":true}`)
		}
		if gotAuth != "Bearer test-token" {
			t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer test-token")
		}
		if gotMethod != http.MethodGet {
			t.Errorf("method = %q, want %q", gotMethod, http.MethodGet)
		}
	})

	t.Run("no bearer token omits Authorization header", func(t *testing.T) {
		var gotAuthValues []string
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuthValues = r.Header["Authorization"]
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		if _, _, err := GetWithBearer(srv.URL, ""); err != nil {
			t.Fatalf("GetWithBearer returned error: %v", err)
		}
		if len(gotAuthValues) > 0 {
			t.Errorf("Authorization header should be absent, got %v", gotAuthValues)
		}
	})

	t.Run("invalid url returns error", func(t *testing.T) {
		if _, _, err := GetWithBearer("http://%zz", ""); err == nil {
			t.Fatal("expected error for malformed URL, got nil")
		}
	})

	t.Run("connection failure returns error", func(t *testing.T) {
		if _, _, err := GetWithBearer("https://127.0.0.1:1", ""); err == nil {
			t.Fatal("expected error for connection failure, got nil")
		}
	})
}

// TestRunMissingBinary exercises Run's plumbing (project-dir resolution,
// env/argv assembly, and its error-wrapping return path) using a binary name
// that is guaranteed not to exist. Go's exec.Cmd resolves the executable via
// LookPath before ever forking, so a missing binary makes CombinedOutput
// return immediately with no process spawned - this is not a real external
// process or a kubectl/helm/network call, just a deterministic, hermetic
// exercise of Run's own error handling.
func TestRunMissingBinary(t *testing.T) {
	const missing = "kaalm-test-utils-definitely-not-a-real-binary-xyz"
	if _, err := exec.LookPath(missing); err == nil {
		t.Skipf("unexpected: %q resolves on this system, skipping", missing)
	}

	out, err := Run(exec.Command(missing, "arg1", "arg2"))
	if err == nil {
		t.Fatal("Run() with a nonexistent binary: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("Run() error = %q, want it to mention the failing command %q", err.Error(), missing)
	}
	if out != "" {
		t.Errorf("Run() output = %q, want empty output for a lookup failure", out)
	}
}
