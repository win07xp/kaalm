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
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cluster"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/console"
	"github.com/win07xp/kaalm/internal/drain"
)

func main() {
	var (
		listenAddr          string
		healthAddr          string
		certFile, keyFile   string
		caFile              string
		gatewayURL          string
		insecureSkipGateway bool
		logLevel            slog.Level
		maxMessageBodyBytes int64
		drainDelay          time.Duration
		shutdownTimeout     time.Duration
	)
	flag.StringVar(&listenAddr, "listen-addr", ":8443", "console listener (pages and read API, TLS)")
	flag.StringVar(&healthAddr, "health-addr", ":8081", "health probe listener")
	flag.StringVar(&certFile, "tls-cert", "/var/run/kaalm/tls.crt", "serving and client certificate (kaalm-console-tls)")
	flag.StringVar(&keyFile, "tls-key", "/var/run/kaalm/tls.key", "certificate key")
	flag.StringVar(&caFile, "tls-ca", "/var/run/kaalm/ca.crt", "Kaalm CA bundle for verifying the gateway")
	flag.StringVar(&gatewayURL, "gateway-url", "",
		"gateway cluster listener base URL (default derived from POD_NAMESPACE)")
	flag.BoolVar(&insecureSkipGateway, "insecure-skip-gateway-verify", false, "skip gateway cert verification (dev only)")
	flag.Int64Var(&maxMessageBodyBytes, "max-message-body-bytes", 1<<20,
		"test-chat request body cap in bytes; larger bodies get 413 (the chart passes gateway.maxMessageBodyBytes)")
	flag.TextVar(&logLevel, "log-level", slog.LevelInfo, "log level: debug, info, warn, or error")
	flag.DurationVar(&drainDelay, "drain-delay", 5*time.Second,
		"after SIGTERM, how long to keep serving while Service endpoints stop sending new connections; 0 shuts down at once")
	flag.DurationVar(&shutdownTimeout, "shutdown-timeout", 30*time.Second,
		"after the drain delay, how long to wait for in-flight requests before exiting")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	// The console logs through package-level slog like the gateway; the JSON
	// convention is docs/src/operations/observability.md's.
	slog.SetDefault(logger)

	if err := drain.Validate(drainDelay, shutdownTimeout); err != nil {
		logger.Error("invalid shutdown flags", "error", err)
		os.Exit(1)
	}

	operatorNamespace := os.Getenv("POD_NAMESPACE")
	if operatorNamespace == "" {
		operatorNamespace = "kaalm-system"
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(kaalmv1beta1.AddToScheme(scheme))

	restCfg := ctrl.GetConfigOrDie()
	// The cached client starts informers lazily per type. The console's RBAC
	// covers exactly the four kaalm.io kinds the data layer reads (Agent,
	// AgentTask, AgentChannel, ModelProvider) and namespaces, and the data
	// layer reads nothing else, so no other informer ever starts.
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
		OperatorNamespace:   operatorNamespace,
		ListenAddr:          listenAddr,
		HealthAddr:          healthAddr,
		CertFile:            certFile,
		KeyFile:             keyFile,
		CAFile:              caFile,
		MaxMessageBodyBytes: maxMessageBodyBytes,
		DrainDelay:          drainDelay,
		ShutdownTimeout:     shutdownTimeout,
	},
		&console.Data{Reader: cl.GetClient()},
		&console.KubeTokenReviewer{Client: clientset},
		console.NewAccessChecker(&console.KubeAuthorizer{Client: clientset}),
		chat,
	)

	// Two contexts: the signal only starts the drain (sigCtx), while the
	// cache the pages read runs on runCtx until the drain has finished.
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	sigCtx, stop := signal.NotifyContext(runCtx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// After the first signal, restore default handling: a second one exits
	// at once instead of waiting for the drain.
	go func() {
		<-sigCtx.Done()
		stop()
	}()

	go func() {
		if err := cl.Start(runCtx); err != nil {
			logger.Error("cluster cache failed", "error", err)
			stop()
		}
	}()
	if !cl.GetCache().WaitForCacheSync(sigCtx) {
		logger.Error("cache sync failed")
		os.Exit(1)
	}

	logger.Info("kaalm-console starting",
		"listenAddr", listenAddr, "healthAddr", healthAddr, "gatewayURL", chat.BaseURL)
	if err := server.Run(sigCtx); err != nil {
		logger.Error("console server failed", "error", err)
		os.Exit(1)
	}
	cancelRun()
}
