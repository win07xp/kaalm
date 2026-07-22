// Command gateway is the Kaalm Gateway: the LLM listener on :8443 with
// per-path client authentication, the provider proxy, and a dedicated health
// port. The User listener (:8080) and the controller-facing internal handlers
// land in later phases. See docs/src/gateways/.
package main

import (
	"context"
	"crypto/x509"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"net/http"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
	"github.com/win07xp/kaalm/internal/callbackpolicy"
	"github.com/win07xp/kaalm/internal/gateway"
)

func main() {
	var (
		listenAddr           string
		healthAddr           string
		certFile, keyFile    string
		caFile               string
		upstreamCAFile       string
		callbackCAFile       string
		callbackAllowlist    string
		maxBodyBytes         int64
		upstreamTimeout      time.Duration
		disableSourceIPCheck bool
		userAddr             string
		agentHostOverride    string
		agentPortOverride    int
		metricsAddr          string
		maxFallbackDepth     int
		maxMessageBodyBytes  int64
		maxResponseBodyBytes int64
		syncDeliveryDeadline time.Duration
		agentReadTimeout     time.Duration
		agentConnectTimeout  time.Duration
		channelHealthWindow  time.Duration
		deliveryBackoff      string
		callbackBackoff      string
	)
	flag.StringVar(&listenAddr, "listen-addr", ":8443", "LLM listener address")
	flag.StringVar(&healthAddr, "health-addr", ":8081", "health listener address")
	flag.StringVar(&certFile, "tls-cert", "/var/run/kaalm/tls.crt", "serving certificate file")
	flag.StringVar(&keyFile, "tls-key", "/var/run/kaalm/tls.key", "serving key file")
	flag.StringVar(&caFile, "tls-ca", "/var/run/kaalm/ca.crt", "Kaalm CA bundle for client verification")
	flag.StringVar(&upstreamCAFile, "upstream-ca", "",
		"optional CA bundle to trust for upstream provider TLS, added to the system roots")
	flag.StringVar(&callbackCAFile, "callback-ca", "",
		"optional CA bundle to trust for channel callbackUrl TLS, added to the system roots")
	flag.StringVar(&callbackAllowlist, "callback-url-allowlist", "",
		"comma-separated DNS-name suffixes and CIDR blocks whose callbackUrl targets are permitted despite the "+
			"deny-internal default; loopback and cloud metadata stay blocked regardless")
	flag.Int64Var(&maxBodyBytes, "max-llm-body-bytes", 4<<20, "inbound LLM request body cap")
	flag.DurationVar(&upstreamTimeout, "upstream-timeout", 120*time.Second, "upstream provider call timeout")
	flag.BoolVar(&disableSourceIPCheck, "disable-source-ip-check", false,
		"skip the source-IP-to-Pod cross-check (dev only; the check is defense in depth and must stay on in-cluster)")
	flag.StringVar(&userAddr, "user-addr", ":8080", "User Gateway listener address")
	flag.StringVar(&agentHostOverride, "agent-host-override", "", "redirect agent delivery dials to this host (dev only)")
	flag.IntVar(&agentPortOverride, "agent-port-override", 0, "redirect agent delivery dials to this port (dev only)")
	flag.StringVar(&metricsAddr, "metrics-addr", ":9090", "Prometheus metrics listener address")
	flag.IntVar(&maxFallbackDepth, "max-fallback-depth", 3, "total providers attempted per request, including the primary")
	flag.Int64Var(&maxMessageBodyBytes, "max-message-body-bytes", 1<<20, "inbound webhook body cap")
	flag.Int64Var(&maxResponseBodyBytes, "max-response-body-bytes", 900<<10, "agent reply body cap")
	flag.DurationVar(&syncDeliveryDeadline, "sync-delivery-deadline", 30*time.Second, "sync-mode wall-clock budget")
	flag.DurationVar(&agentReadTimeout, "agent-read-timeout", 10*time.Second, "per-attempt agent/callback read timeout")
	flag.DurationVar(&agentConnectTimeout, "agent-connect-timeout", time.Second, "agent-delivery connect timeout")
	flag.DurationVar(&channelHealthWindow, "channel-health-window", 5*time.Minute, "rolling window for PlatformConnected")
	flag.StringVar(&deliveryBackoff, "delivery-backoff", "1s,5s,25s", "agent-delivery retry backoff (comma-separated)")
	flag.StringVar(&callbackBackoff, "callback-backoff", "1s,5s,25s", "callback retry backoff schedule (comma-separated)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	operatorNamespace := os.Getenv("POD_NAMESPACE")
	if operatorNamespace == "" {
		operatorNamespace = "kaalm-system"
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(kaalmv1alpha1.AddToScheme(scheme))
	utilruntime.Must(cmapi.AddToScheme(scheme))

	restCfg := ctrl.GetConfigOrDie()
	// Secrets are read uncached (direct GET), never through an informer. The
	// gateway holds only get/watch on Secrets in kaalm-system plus dynamic
	// resourceNames-scoped grants on individual channel Secrets (no cluster-wide
	// list), so a cached Secret informer would issue a forbidden cluster-scoped
	// LIST and the read would hang waiting for a sync that never lands. See
	// docs/src/security/rbac.md (gateway Secret access).
	cl, err := cluster.New(restCfg, func(o *cluster.Options) {
		o.Scheme = scheme
		o.Client.Cache = &client.CacheOptions{
			DisableFor: []client.Object{&corev1.Secret{}},
		}
	})
	if err != nil {
		logger.Error("building cluster cache", "error", err)
		os.Exit(1)
	}
	if err := cl.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, gateway.PodIPIndex,
		func(obj client.Object) []string {
			pod := obj.(*corev1.Pod)
			if pod.Status.PodIP == "" {
				return nil
			}
			return []string{pod.Status.PodIP}
		}); err != nil {
		logger.Error("registering pod IP index", "error", err)
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		logger.Error("building clientset", "error", err)
		os.Exit(1)
	}

	// The upstream and callback CA bundles are additive to the system roots:
	// they let the gateway reach in-cluster or self-hosted providers/receivers
	// signed by a private CA (e.g. kaalm-ca) without losing the ability to
	// verify public endpoints. The server re-reads them when their mtime
	// changes, so a rotated bundle needs no restart; validate once here so a
	// misconfigured path fails fast at startup rather than at first dial.
	validateCABundle(upstreamCAFile, "upstream", logger)
	validateCABundle(callbackCAFile, "callback", logger)

	store := &gateway.KubeStore{Reader: cl.GetClient(), OperatorNamespace: operatorNamespace}
	tokens := gateway.NewTokenAuthenticator(&gateway.KubeTokenReviewer{Client: clientset})
	async := &gateway.KubeAsyncRecords{Client: clientset, OperatorNamespace: operatorNamespace}
	server := gateway.NewServer(gateway.Config{
		OperatorNamespace:        operatorNamespace,
		ListenAddr:               listenAddr,
		HealthAddr:               healthAddr,
		CertFile:                 certFile,
		KeyFile:                  keyFile,
		CAFile:                   caFile,
		MaxBodyBytes:             maxBodyBytes,
		UpstreamTimeout:          upstreamTimeout,
		UpstreamCAFile:           upstreamCAFile,
		CallbackCAFile:           callbackCAFile,
		CallbackPolicy:           callbackpolicy.NewFromCSV(callbackAllowlist),
		DisableSourceIPCheck:     disableSourceIPCheck,
		UserListenAddr:           userAddr,
		AgentServiceHostOverride: agentHostOverride,
		AgentServicePortOverride: int32(agentPortOverride),
		MaxFallbackDepth:         maxFallbackDepth,
		MaxMessageBodyBytes:      maxMessageBodyBytes,
		MaxResponseBodyBytes:     maxResponseBodyBytes,
		SyncDeliveryDeadline:     syncDeliveryDeadline,
		AgentReadTimeout:         agentReadTimeout,
		AgentConnectTimeout:      agentConnectTimeout,
		ChannelHealthWindow:      channelHealthWindow,
		DeliveryBackoff:          parseBackoff(deliveryBackoff, logger),
		CallbackBackoff:          parseBackoff(callbackBackoff, logger),
		Replicas: func() int {
			var pods corev1.PodList
			if err := cl.GetClient().List(context.Background(), &pods,
				client.InNamespace(operatorNamespace),
				client.MatchingLabels{"app.kubernetes.io/component": "gateway"}); err != nil || len(pods.Items) == 0 {
				return 1
			}
			return len(pods.Items)
		},
	}, store, tokens, gateway.NewMemorySpend())
	server.Async = async
	server.Completions = &gateway.KubeCompletionWriter{Client: clientset}
	server.Metrics = gateway.NewMetrics(metrics.Registry)
	server.Recorder = cl.GetEventRecorderFor("kaalm-gateway")

	// Prometheus metrics on a dedicated unauthenticated in-cluster port.
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))
		srv := &http.Server{Addr: metricsAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("metrics listener failed", "error", err)
		}
	}()
	if activatorClient, err := gateway.NewControllerActivator(
		operatorNamespace, certFile, keyFile, caFile); err == nil {
		server.Activator = activatorClient
	} else {
		logger.Info("activator client disabled", "reason", err.Error())
	}

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

	// The budget counter exchange: publish this replica's partials and fold
	// peers' on a 10s cadence. POD_NAME comes from the downward API.
	podName := os.Getenv("POD_NAME")
	if podName == "" {
		podName, _ = os.Hostname()
	}
	publisher := &gateway.BudgetPublisher{
		Client: clientset, Store: store, Ledger: server.Budget,
		OperatorNamespace: operatorNamespace, PodName: podName,
		Providers: func(ctx context.Context) []*kaalmv1alpha1.ModelProvider {
			var list kaalmv1alpha1.ModelProviderList
			if err := cl.GetClient().List(ctx, &list); err != nil {
				return nil
			}
			out := make([]*kaalmv1alpha1.ModelProvider, 0, len(list.Items))
			for i := range list.Items {
				out = append(out, &list.Items[i])
			}
			return out
		},
	}
	publisher.SeedFromCanonical(ctx)
	go publisher.Run(ctx)

	// The gateway's half of the channel-delete handshake: once a channel is
	// observed Terminating, confirm disconnection with the annotation the
	// reconciler waits on. The webhook write gate itself lives in the intake
	// handler.
	if informer, err := cl.GetCache().GetInformer(ctx, &kaalmv1alpha1.AgentChannel{}); err == nil {
		_, _ = informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
			UpdateFunc: func(_, newObj any) {
				ch, ok := newObj.(*kaalmv1alpha1.AgentChannel)
				if !ok || ch.Status.Phase != kaalmv1alpha1.ChannelTerminating {
					return
				}
				if ch.Annotations[kaalmv1alpha1.AnnotationChannelDisconnected] == kaalmv1alpha1.AnnotationTrue {
					return
				}
				patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`,
					kaalmv1alpha1.AnnotationChannelDisconnected, kaalmv1alpha1.AnnotationTrue))
				if err := cl.GetClient().Patch(ctx, ch.DeepCopy(),
					client.RawPatch(types.MergePatchType, patch)); err != nil {
					logger.Warn("disconnect annotation patch failed", "channel", ch.Name, "error", err)
				}
			},
		})
	}

	logger.Info("kaalm gateway starting",
		"listen", listenAddr, "health", healthAddr, "operator_namespace", operatorNamespace,
		"source_ip_check_disabled", disableSourceIPCheck)
	if err := server.Run(ctx); err != nil {
		logger.Error("gateway listener failed", "error", err)
		os.Exit(1)
	}
	logger.Info("kaalm gateway shut down")
}

// parseBackoff parses a comma-separated duration schedule like "1s,5s,25s".
// An empty or malformed value falls back to the Config default (nil).
// validateCABundle checks that the PEM bundle at file exists and parses, and
// exits if not: a misconfigured trust bundle must not be silently ignored. The
// pool itself is built (and rebuilt on rotation) by the server. kind names the
// bundle in log messages; an empty file is a no-op.
func validateCABundle(file, kind string, logger *slog.Logger) {
	if file == "" {
		return
	}
	pem, err := os.ReadFile(file)
	if err != nil {
		logger.Error("reading "+kind+" CA bundle", "error", err)
		os.Exit(1)
	}
	if !x509.NewCertPool().AppendCertsFromPEM(pem) {
		logger.Error("no certificates parsed from " + kind + " CA bundle")
		os.Exit(1)
	}
}

func parseBackoff(raw string, logger *slog.Logger) []time.Duration {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]time.Duration, 0, len(parts))
	for _, part := range parts {
		d, err := time.ParseDuration(strings.TrimSpace(part))
		if err != nil {
			logger.Warn("ignoring malformed backoff entry", "value", part, "error", err)
			continue
		}
		out = append(out, d)
	}
	return out
}
