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

// The kaalm-console binary: the optional operator console
// (docs/src/console/overview.md). One TLS listener serving the read API and
// the server-rendered pages, a health port, and nothing else. Off by
// default; the chart creates this Deployment only with console.enabled.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cluster"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
	"github.com/win07xp/kaalm/internal/console"
)

func main() {
	var (
		listenAddr          string
		healthAddr          string
		certFile, keyFile   string
		caFile              string
		gatewayURL          string
		insecureSkipGateway bool
	)
	flag.StringVar(&listenAddr, "listen-addr", ":8443", "console listener (pages and read API, TLS)")
	flag.StringVar(&healthAddr, "health-addr", ":8081", "health probe listener")
	flag.StringVar(&certFile, "tls-cert", "/var/run/kaalm/tls.crt", "serving and client certificate (kaalm-console-tls)")
	flag.StringVar(&keyFile, "tls-key", "/var/run/kaalm/tls.key", "certificate key")
	flag.StringVar(&caFile, "tls-ca", "/var/run/kaalm/ca.crt", "Kaalm CA bundle for verifying the gateway")
	flag.StringVar(&gatewayURL, "gateway-url", "",
		"gateway cluster listener base URL (default derived from POD_NAMESPACE)")
	flag.BoolVar(&insecureSkipGateway, "insecure-skip-gateway-verify", false, "skip gateway cert verification (dev only)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// The console logs through package-level slog like the gateway; the JSON
	// convention is docs/src/operations/observability.md's.
	slog.SetDefault(logger)

	operatorNamespace := os.Getenv("POD_NAMESPACE")
	if operatorNamespace == "" {
		operatorNamespace = "kaalm-system"
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(kaalmv1alpha1.AddToScheme(scheme))

	restCfg := ctrl.GetConfigOrDie()
	// The cached client starts informers lazily per type. The console's RBAC
	// covers exactly the six kaalm.io CRDs and namespaces, and the data layer
	// reads nothing else, so no other informer ever starts.
	cl, err := cluster.New(restCfg, func(o *cluster.Options) {
		o.Scheme = scheme
	})
	if err != nil {
		logger.Error("building cluster cache", "error", err)
		os.Exit(1)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		logger.Error("building clientset", "error", err)
		os.Exit(1)
	}

	chat := console.NewGatewayChatClient(operatorNamespace, certFile, keyFile, caFile)
	if gatewayURL != "" {
		chat.BaseURL = gatewayURL
	}
	if insecureSkipGateway {
		chat.Insecure = true
		logger.Warn("gateway certificate verification is disabled; dev use only")
	}

	server := console.NewServer(console.Config{
		OperatorNamespace: operatorNamespace,
		ListenAddr:        listenAddr,
		HealthAddr:        healthAddr,
		CertFile:          certFile,
		KeyFile:           keyFile,
		CAFile:            caFile,
	},
		&console.Data{Reader: cl.GetClient()},
		&console.KubeTokenReviewer{Client: clientset},
		console.NewGate(&console.KubeAuthorizer{Client: clientset}),
		chat,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := cl.Start(ctx); err != nil {
			logger.Error("cluster cache failed", "error", err)
			stop()
		}
	}()
	if !cl.GetCache().WaitForCacheSync(ctx) {
		logger.Error("cache sync failed")
		os.Exit(1)
	}

	logger.Info("kaalm-console starting",
		"listenAddr", listenAddr, "healthAddr", healthAddr, "gatewayURL", chat.BaseURL)
	if err := server.Run(ctx); err != nil {
		logger.Error("console server failed", "error", err)
		os.Exit(1)
	}
}
