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
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
)

// holdPort binds a loopback TCP socket to a free port without listening on
// it. While held, a connect to the port is refused (no listener, so the kernel
// answers with a reset) and no other socket can bind it: on Linux a bind that
// overlaps a bound socket without SO_REUSEADDR fails, and the free-port search
// of a Listen on port 0 skips it. A Listen-then-Close reservation leaves the
// port free for another process in between, and a listener held open would
// accept connects into its backlog. BSD bind rules differ; CI and development
// run on Linux. listen turns the same socket into a listener.
func holdPort(t *testing.T) (port int, listen func() (net.Listener, error)) {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	syscall.CloseOnExec(fd)
	var closeOnce sync.Once
	closeFD := func() { closeOnce.Do(func() { _ = syscall.Close(fd) }) }
	t.Cleanup(closeFD)
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	port = sa.(*syscall.SockaddrInet4).Port
	return port, func() (net.Listener, error) {
		if err := syscall.Listen(fd, syscall.SOMAXCONN); err != nil {
			return nil, err
		}
		var ln net.Listener
		var err error
		// FileListener dups the descriptor; closing the file closes the
		// original, so the Cleanup close must not run on it afterwards.
		closeOnce.Do(func() {
			f := os.NewFile(uintptr(fd), "held-port")
			ln, err = net.FileListener(f)
			_ = f.Close()
		})
		return ln, err
	}
}
