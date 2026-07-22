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
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:golint,revive
)

// Run executes the provided command within this context
func Run(cmd *exec.Cmd) (string, error) {
	dir, _ := GetProjectDir()
	cmd.Dir = dir

	if err := os.Chdir(cmd.Dir); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "chdir dir: %s\n", err)
	}

	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %s\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%s failed with error: (%v) %s", command, err, string(output))
	}

	return string(output), nil
}

// GetNonEmptyLines converts given command output string into individual objects
// according to line breakers, and ignores the empty elements in it.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.Split(output, "\n")
	for _, element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

// GetProjectDir will return the directory where the project is
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, err
	}
	wd = strings.Replace(wd, "/test/e2e", "", -1)
	return wd, nil
}

// Kubectl runs kubectl with the given args from the project root.
func Kubectl(args ...string) (string, error) {
	return Run(exec.Command("kubectl", args...))
}

// Helm runs helm with the given args from the project root.
func Helm(args ...string) (string, error) {
	return Run(exec.Command("helm", args...))
}

// WaitRollout blocks until a Deployment's rollout completes or the timeout fires.
func WaitRollout(namespace, deploy, timeout string) error {
	_, err := Kubectl("rollout", "status", "deploy/"+deploy, "-n", namespace, "--timeout", timeout)
	return err
}

// ResourceField reads a single jsonpath field off a resource. namespace may be
// "" for cluster-scoped kinds.
func ResourceField(kind, namespace, name, jsonpath string) (string, error) {
	args := []string{"get", kind, name, "-o", "jsonpath=" + jsonpath}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	return Kubectl(args...)
}

// parseForwardPort extracts the local port kubectl prints for a dynamic
// port-forward. It matches only the IPv4 line so the IPv6 duplicate is ignored.
func parseForwardPort(line string) (int, bool) {
	const marker = "Forwarding from 127.0.0.1:"
	i := strings.Index(line, marker)
	if i < 0 {
		return 0, false
	}
	rest := line[i+len(marker):]
	j := strings.IndexByte(rest, ' ')
	if j < 0 {
		return 0, false
	}
	p, err := strconv.Atoi(rest[:j])
	if err != nil {
		return 0, false
	}
	return p, true
}

// PortForward starts `kubectl port-forward svc/<svc> :<remotePort>` with a
// kubectl-chosen local port (avoids collisions), returning the local port and a
// stop func. Caller must call stop().
func PortForward(namespace, svc, remotePort string) (int, func(), error) {
	cmd := exec.Command("kubectl", "port-forward", "-n", namespace, "svc/"+svc, ":"+remotePort)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, nil, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}
	stop := func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }

	portCh := make(chan int, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if p, ok := parseForwardPort(scanner.Text()); ok {
				portCh <- p
				// Keep draining kubectl's combined stdout+stderr so a long-lived
				// forward never blocks on a full OS pipe buffer. The pipe closes
				// on EOF when stop() kills the process.
				_, _ = io.Copy(io.Discard, stdout)
				return
			}
		}
		portCh <- 0
	}()

	select {
	case p := <-portCh:
		if p == 0 {
			stop()
			return 0, nil, fmt.Errorf("port-forward did not report a local port")
		}
		return p, stop, nil
	case <-time.After(15 * time.Second):
		stop()
		return 0, nil, fmt.Errorf("port-forward timed out waiting for a local port")
	}
}

// PostJSON POSTs a JSON body over HTTPS, skipping cert verification (the target
// is a localhost port-forward in the e2e suite only).
func PostJSON(url, bearer string, body []byte) (int, string, error) {
	return PostJSONHeaders(url, bearer, nil, body)
}

// PostJSONHeaders is PostJSON with extra request headers (for example the
// userId header a channel extracts with fromHeader).
func PostJSONHeaders(url, bearer string, headers map[string]string, body []byte) (int, string, error) {
	tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // localhost port-forward only
	tr := &http.Transport{TLSClientConfig: tlsCfg}
	cli := &http.Client{Timeout: 45 * time.Second, Transport: tr}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := cli.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, string(b), nil
}

// GetWithBearer issues a GET with an optional bearer token, mirroring PostJSON.
// Used by the async polling endpoint assertions.
func GetWithBearer(url, bearer string) (int, string, error) {
	tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // localhost port-forward only
	tr := &http.Transport{TLSClientConfig: tlsCfg}
	cli := &http.Client{Timeout: 45 * time.Second, Transport: tr}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := cli.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, string(b), nil
}
