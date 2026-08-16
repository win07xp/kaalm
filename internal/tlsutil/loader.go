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

// Package tlsutil holds the rotation-aware certificate and CA-bundle loaders
// shared by the gateway and the console. cert-manager rotates the mounted
// files in place; the loaders re-read on mtime change and keep the previous
// value through a partial write, so no component needs a restart to pick up
// a rotated cert.
package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sync"
	"time"
)

// CAPoolLoader reloads a CA bundle from disk when its mtime changes, keeping
// the previous pool through a partial write. Additive adds the bundle to the
// system roots instead of replacing them, which is what outbound (upstream
// provider and channel callback) trust pools need; an inbound listener pool
// verifies only against the Kaalm CA and is not additive.
type CAPoolLoader struct {
	// Files are merged into one pool, so the cluster CA and an
	// operator-supplied enterprise bundle can be trusted together (a projected
	// volume cannot concatenate two ConfigMap keys into a single file).
	Files    []string
	Additive bool

	mu     sync.Mutex
	pool   *x509.CertPool
	mtimes []time.Time
}

// Load returns the current pool, re-reading the bundle files when any mtime
// changed.
func (l *CAPoolLoader) Load() (*x509.CertPool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	mtimes := make([]time.Time, len(l.Files))
	for i, file := range l.Files {
		info, err := os.Stat(file)
		if err != nil {
			if l.pool != nil {
				return l.pool, nil
			}
			return nil, err
		}
		mtimes[i] = info.ModTime()
	}
	if l.pool != nil && sameMtimes(l.mtimes, mtimes) {
		return l.pool, nil
	}

	pool := x509.NewCertPool()
	if l.Additive {
		if system, sysErr := x509.SystemCertPool(); sysErr == nil && system != nil {
			pool = system
		}
	}
	for _, file := range l.Files {
		pem, err := os.ReadFile(file)
		if err != nil {
			if l.pool != nil {
				return l.pool, nil
			}
			return nil, err
		}
		if !pool.AppendCertsFromPEM(pem) {
			if l.pool != nil {
				return l.pool, nil
			}
			return nil, fmt.Errorf("no certificates parsed from CA bundle %s", file)
		}
	}
	l.pool, l.mtimes = pool, mtimes
	return l.pool, nil
}

func sameMtimes(a, b []time.Time) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			return false
		}
	}
	return true
}

// CertLoader reloads a serving-or-client cert and its CA bundle from disk
// when their mtimes change, so cert-manager rotation needs no process
// restart.
type CertLoader struct {
	CertFile, KeyFile, CAFile string

	mu        sync.Mutex
	cert      *tls.Certificate
	certMtime time.Time
	caOnce    sync.Once
	ca        *CAPoolLoader
}

// Certificate returns the current key pair, re-reading it on mtime change.
func (l *CertLoader) Certificate() (*tls.Certificate, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	info, err := os.Stat(l.CertFile)
	if err != nil {
		return nil, err
	}
	if l.cert != nil && info.ModTime().Equal(l.certMtime) {
		return l.cert, nil
	}
	cert, err := tls.LoadX509KeyPair(l.CertFile, l.KeyFile)
	if err != nil {
		if l.cert != nil {
			return l.cert, nil // keep serving the old cert through a partial write
		}
		return nil, err
	}
	l.cert, l.certMtime = &cert, info.ModTime()
	return l.cert, nil
}

// CAPool returns the current CA pool for CAFile, with the same reload rules.
func (l *CertLoader) CAPool() (*x509.CertPool, error) {
	l.caOnce.Do(func() { l.ca = &CAPoolLoader{Files: []string{l.CAFile}} })
	return l.ca.Load()
}
