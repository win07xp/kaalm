// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// certReloader holds the agent's TLS material and rebuilds it when the kubelet
// rotates the projected volume. It watches the mount DIRECTORY, not the leaf
// files: the kubelet rotates by atomically renaming the ..data symlink, so a
// leaf-path watch never fires (runtime contract item 4).
type certReloader struct {
	certFile, keyFile, caFile string

	mu     sync.RWMutex
	cert   *tls.Certificate
	caPool *x509.CertPool // both the inbound ClientCAs and the outbound RootCAs
}

func newCertReloader(certFile, keyFile, caFile string) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile, caFile: caFile}
	if err := r.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *certReloader) reload() error {
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("loading cert/key: %w", err)
	}
	caPEM, err := os.ReadFile(r.caFile)
	if err != nil {
		return fmt.Errorf("reading CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("no certificates parsed from %s", r.caFile)
	}
	r.mu.Lock()
	r.cert, r.caPool = &cert, pool
	r.mu.Unlock()
	return nil
}

func (r *certReloader) certificate() *tls.Certificate {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cert
}

func (r *certReloader) pool() *x509.CertPool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.caPool
}

// watch anchors an fsnotify watcher on the mount directory and reloads on any
// change to the ..data entry. Runs until the process exits.
func (r *certReloader) watch(logf func(string, ...any)) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	dir := filepath.Dir(r.certFile)
	if err := watcher.Add(dir); err != nil {
		return err
	}
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				// The kubelet swaps ..data via CREATE/rename.
				if filepath.Base(event.Name) == "..data" &&
					event.Op&(fsnotify.Create|fsnotify.Rename) != 0 {
					if err := r.reload(); err != nil {
						logf("cert reload failed: %v", err)
					} else {
						logf("reloaded TLS material after rotation")
					}
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				logf("cert watch error: %v", err)
			}
		}
	}()
	return nil
}

// serverTLSConfig requests a client cert without requiring one so kubelet
// probes (which present none) still complete the handshake, and consults the
// reloader on each handshake so a rotation takes effect without a restart.
func (r *certReloader) serverTLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		ClientAuth: tls.VerifyClientCertIfGiven,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return r.certificate(), nil
		},
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			// A CA-bundle rotation must refresh the inbound ClientCAs pool,
			// not only the serving cert.
			return &tls.Config{
				MinVersion: tls.VersionTLS12,
				ClientAuth: tls.VerifyClientCertIfGiven,
				ClientCAs:  r.pool(),
				GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
					return r.certificate(), nil
				},
			}, nil
		},
	}
}

// clientTLSConfig presents the agent cert and trusts the CA for outbound
// gateway calls; both are pulled live from the reloader on each dial.
func (r *certReloader) clientTLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return r.certificate(), nil
		},
		RootCAs: r.pool(),
	}
}
