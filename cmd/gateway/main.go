// Command gateway is the Kaalm Gateway: the cluster listener on :8443 (the
// LLM proxy, the MCP tool broker, and the internal endpoints, with per-path
// client authentication), the Ingress-fronted user listener on :8080, and a
// dedicated health port. See docs/src/gateways/.
package main

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"net/http"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/callbackpolicy"
	"github.com/win07xp/kaalm/internal/drain"
	"github.com/win07xp/kaalm/internal/eventrec"
	"github.com/win07xp/kaalm/internal/gateway"
	"github.com/win07xp/kaalm/internal/profiling"
	"github.com/win07xp/kaalm/internal/secretwatch"
	"github.com/win07xp/kaalm/internal/tlsutil"
)

// gatewayFlags holds the gateway's command-line flags. registerFlags fills
// it from a FlagSet, so a test can parse flags into one and check what main
// does with them.
type gatewayFlags struct {
	listenAddr           string
	healthAddr           string
	certFile, keyFile    string
	caFile               string
	upstreamCAFile       string
	callbackCAFile       string
	callbackAllowlist    string
	maxBodyBytes         int64
	upstreamTimeout      time.Duration
	mcpMaxBodyBytes      int64
	mcpUpstreamTimeout   time.Duration
	disableSourceIPCheck bool
	userAddr             string
	agentHostOverride    string
	agentPortOverride    int
	metricsAddr          string
	pprofAddr            string
	otlpEndpoint         string
	otlpSampleRatio      float64
	maxFallbackDepth     int
	maxMessageBodyBytes  int64
	maxResponseBodyBytes int64
	syncDeliveryDeadline time.Duration
	maxWakeTimeout       time.Duration
	agentReadTimeout     time.Duration
	callbackReadTimeout  time.Duration
	agentConnectTimeout  time.Duration
	channelHealthWindow  time.Duration
	deliveryBackoff      string
	callbackBackoff      string
	discordAPIBaseURL    string
	whatsAppAPIBaseURL   string
	logLevel             slog.Level
	clientQPS            float64
	clientBurst          int
	drainDelay           time.Duration
	shutdownTimeout      time.Duration
}

// registerFlags registers every gateway flag on fs and returns the struct
// fs.Parse fills.
func registerFlags(fs *flag.FlagSet) *gatewayFlags {
	f := &gatewayFlags{}
	fs.StringVar(&f.listenAddr, "listen-addr", ":8443", "cluster listener (:8443) address")
	fs.StringVar(&f.healthAddr, "health-addr", ":8081", "health listener address")
	fs.StringVar(&f.certFile, "tls-cert", "/var/run/kaalm/tls.crt", "serving certificate file")
	fs.StringVar(&f.keyFile, "tls-key", "/var/run/kaalm/tls.key", "serving key file")
	fs.StringVar(&f.caFile, "tls-ca", "/var/run/kaalm/ca.crt", "Kaalm CA bundle for client verification")
	fs.StringVar(&f.upstreamCAFile, "upstream-ca", "",
		"comma-separated CA bundle files to trust for upstream provider TLS, merged and added to the system roots")
	fs.StringVar(&f.callbackCAFile, "callback-ca", "",
		"comma-separated CA bundle files to trust for channel callbackUrl TLS, merged and added to the system roots")
	fs.StringVar(&f.callbackAllowlist, "callback-url-allowlist", "",
		"comma-separated DNS-name suffixes and CIDR blocks whose callbackUrl targets are permitted despite the "+
			"deny-internal default; loopback and cloud metadata stay blocked regardless")
	fs.Int64Var(&f.maxBodyBytes, "max-llm-body-bytes", 4<<20, "inbound LLM request body cap")
	fs.DurationVar(&f.upstreamTimeout, "upstream-timeout", 120*time.Second,
		"per-attempt upstream provider bound: the wait for the first response byte, then each gap between body reads")
	fs.Int64Var(&f.mcpMaxBodyBytes, "mcp-max-body-bytes", 4<<20, "brokered MCP request and response body cap")
	fs.DurationVar(&f.mcpUpstreamTimeout, "mcp-upstream-timeout", 120*time.Second, "brokered MCP whole-call timeout")
	fs.BoolVar(&f.disableSourceIPCheck, "disable-source-ip-check", false,
		"skip the source-IP-to-Pod cross-check (dev only; the check is defense in depth and must stay on in-cluster)")
	fs.StringVar(&f.userAddr, "user-addr", ":8080", "User Gateway listener address")
	fs.StringVar(&f.agentHostOverride, "agent-host-override", "", "redirect agent delivery dials to this host (dev only)")
	fs.IntVar(&f.agentPortOverride, "agent-port-override", 0, "redirect agent delivery dials to this port (dev only)")
	fs.StringVar(&f.metricsAddr, "metrics-addr", ":9090", "Prometheus metrics listener address")
	fs.StringVar(&f.pprofAddr, "pprof-addr", "",
		"net/http/pprof listener address (debug only; empty disables it)")
	fs.StringVar(&f.otlpEndpoint, "otlp-endpoint", "",
		"OTLP/HTTP trace exporter base URL (for example http://collector:4318); empty disables tracing entirely")
	fs.Float64Var(&f.otlpSampleRatio, "otlp-sample-ratio", 1.0,
		"parent-based head sampling ratio for traces this gateway starts")
	fs.IntVar(&f.maxFallbackDepth, "max-fallback-depth", 3, "total providers attempted per request, including the primary")
	fs.Int64Var(&f.maxMessageBodyBytes, "max-message-body-bytes", 1<<20, "inbound webhook body cap")
	fs.Int64Var(&f.maxResponseBodyBytes, "max-response-body-bytes", 900<<10, "agent reply body cap")
	fs.DurationVar(&f.syncDeliveryDeadline, "sync-delivery-deadline", 30*time.Second, "sync-mode wall-clock budget")
	fs.DurationVar(&f.maxWakeTimeout, "max-wake-timeout", 15*time.Minute,
		"longest the gateway waits for a woken Agent to become reachable, whatever its wakeTimeout")
	fs.DurationVar(&f.agentReadTimeout, "agent-read-timeout", 10*time.Second, "per-attempt agent read timeout")
	fs.DurationVar(&f.callbackReadTimeout, "callback-read-timeout", 10*time.Second, "per-attempt callback read timeout")
	fs.DurationVar(&f.agentConnectTimeout, "agent-connect-timeout", time.Second, "agent-delivery connect timeout")
	fs.DurationVar(&f.channelHealthWindow, "channel-health-window", 5*time.Minute, "rolling window for PlatformConnected")
	fs.StringVar(&f.deliveryBackoff, "delivery-backoff", "1s,5s,25s", "agent-delivery retry backoff (comma-separated)")
	fs.StringVar(&f.callbackBackoff, "callback-backoff", "1s,5s,25s", "callback retry backoff schedule (comma-separated)")
	fs.StringVar(&f.discordAPIBaseURL, "platform-discord-api-base-url", gateway.DefaultDiscordAPIBaseURL,
		"base URL the Discord adapter replies through (gateway.platforms.discord.apiBaseUrl)")
	fs.StringVar(&f.whatsAppAPIBaseURL, "platform-whatsapp-api-base-url", gateway.DefaultWhatsAppAPIBaseURL,
		"base URL the WhatsApp adapter replies through, including the Graph API version "+
			"(gateway.platforms.whatsapp.apiBaseUrl)")
	fs.TextVar(&f.logLevel, "log-level", slog.LevelInfo, "log level: debug, info, warn, or error")
	fs.Float64Var(&f.clientQPS, "client-qps", 100, "Kubernetes API client sustained requests per second")
	fs.IntVar(&f.clientBurst, "client-burst", 200, "Kubernetes API client burst above --client-qps")
	fs.DurationVar(&f.drainDelay, "drain-delay", 5*time.Second,
		"after SIGTERM, how long to keep serving while Services and ingress controllers "+
			"stop routing new connections to this Pod; 0 skips the wait")
	fs.DurationVar(&f.shutdownTimeout, "shutdown-timeout", 30*time.Second,
		"after the drain delay, how long to wait for in-flight requests and background deliveries "+
			"before exiting; 0 waits for none of them")
	return f
}

func main() {
	f := registerFlags(flag.CommandLine)
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: f.logLevel}))
	// Install it as the process default: the gateway packages log through
	// package-level slog (including the broker's per-call audit record), and
	// without this they would fall back to Go's plain text logger, breaking
	// the JSON log convention (docs/src/operations/observability.md).
	slog.SetDefault(logger)

	if err := drain.Validate(f.drainDelay, f.shutdownTimeout); err != nil {
		logger.Error("invalid shutdown flags", "error", err)
		os.Exit(1)
	}
	if err := validateLimits(limitFlags(f)); err != nil {
		logger.Error("invalid limit flags", "error", err)
		os.Exit(1)
	}
	if err := validateMaxWakeTimeout(f.maxWakeTimeout); err != nil {
		logger.Error("invalid --max-wake-timeout", "error", err)
		os.Exit(1)
	}

	operatorNamespace := os.Getenv("POD_NAMESPACE")
	if operatorNamespace == "" {
		operatorNamespace = "kaalm-system"
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(kaalmv1beta1.AddToScheme(scheme))
	utilruntime.Must(cmapi.AddToScheme(scheme))

	restCfg := ctrl.GetConfigOrDie()
	// The controller-runtime defaults (20 QPS, burst 30) size a reconciler's
	// client, not a proxy's. The gateway's live reads (task-completion
	// cross-checks, first-use Secret loads) must never queue behind a bucket
	// that small: at 20 QPS a per-request GET capped every replica at 20
	// requests per second. The defaults are 100 and 200.
	restCfg.QPS = float32(f.clientQPS)
	restCfg.Burst = f.clientBurst
	cl, err := cluster.New(restCfg, clusterOptions(scheme, operatorNamespace))
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
	if err := cl.GetFieldIndexer().IndexField(context.Background(), &kaalmv1beta1.AgentChannel{},
		gateway.ChannelPathIndex, gateway.ChannelPathIndexValue); err != nil {
		logger.Error("registering channel path index", "error", err)
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
	for _, file := range splitPaths(f.upstreamCAFile) {
		validateCABundle(file, "upstream", logger)
	}
	for _, file := range splitPaths(f.callbackCAFile) {
		validateCABundle(file, "callback", logger)
	}

	callbackPolicy := parseStartupPolicy(f.callbackAllowlist, f.discordAPIBaseURL, f.whatsAppAPIBaseURL, logger)

	// The MCP session key binds session ids to caller identities across
	// replicas; the chart provides it. A missing key gets a per-process
	// random fallback: sessions then survive only on this replica, which is
	// fine for local runs and loudly wrong for real ones.
	sessionKey := []byte(os.Getenv("KAALM_MCP_SESSION_KEY"))
	if len(sessionKey) == 0 {
		sessionKey = make([]byte, 32)
		if _, err := rand.Read(sessionKey); err != nil {
			logger.Error("generating fallback MCP session key", "error", err)
			os.Exit(1)
		}
		logger.Warn("KAALM_MCP_SESSION_KEY is not set; using a per-process key, " +
			"MCP sessions will not survive replica hops")
	}

	store := &gateway.KubeStore{
		Reader: cl.GetClient(), APIReader: cl.GetAPIReader(), OperatorNamespace: operatorNamespace,
	}
	tokens := gateway.NewTokenAuthenticator(&gateway.KubeTokenReviewer{Client: clientset})
	async := &gateway.KubeAsyncRecords{Client: clientset, OperatorNamespace: operatorNamespace, Reader: cl.GetClient()}
	// Both counts are read on every request; a listing per request was
	// measurable under load, and counts a few seconds stale are within the
	// margin the rate limiter and the hard budget already carry.
	replicas := gateway.CachedCount(5*time.Second, func() gatewayReplicas {
		return countGatewayReplicas(context.Background(), cl.GetClient(), operatorNamespace)
	})
	server := gateway.NewServer(gateway.Config{
		OperatorNamespace:        operatorNamespace,
		ListenAddr:               f.listenAddr,
		HealthAddr:               f.healthAddr,
		CertFile:                 f.certFile,
		KeyFile:                  f.keyFile,
		CAFile:                   f.caFile,
		MaxBodyBytes:             f.maxBodyBytes,
		UpstreamTimeout:          f.upstreamTimeout,
		MCPMaxBodyBytes:          f.mcpMaxBodyBytes,
		MCPUpstreamTimeout:       f.mcpUpstreamTimeout,
		SessionKey:               sessionKey,
		UpstreamCAFiles:          splitPaths(f.upstreamCAFile),
		CallbackCAFiles:          splitPaths(f.callbackCAFile),
		CallbackPolicy:           callbackPolicy,
		DisableSourceIPCheck:     f.disableSourceIPCheck,
		UserListenAddr:           f.userAddr,
		AgentServiceHostOverride: f.agentHostOverride,
		AgentServicePortOverride: int32(f.agentPortOverride),
		MaxFallbackDepth:         f.maxFallbackDepth,
		MaxMessageBodyBytes:      f.maxMessageBodyBytes,
		MaxResponseBodyBytes:     f.maxResponseBodyBytes,
		SyncDeliveryDeadline:     f.syncDeliveryDeadline,
		MaxWakeTimeout:           f.maxWakeTimeout,
		AgentReadTimeout:         f.agentReadTimeout,
		CallbackReadTimeout:      f.callbackReadTimeout,
		AgentConnectTimeout:      f.agentConnectTimeout,
		ChannelHealthWindow:      f.channelHealthWindow,
		DeliveryBackoff:          mustParseBackoff(f.deliveryBackoff, "delivery-backoff", logger),
		CallbackBackoff:          mustParseBackoff(f.callbackBackoff, "callback-backoff", logger),
		DiscordAPIBaseURL:        f.discordAPIBaseURL,
		WhatsAppAPIBaseURL:       f.whatsAppAPIBaseURL,
		DrainDelay:               f.drainDelay,
		ShutdownTimeout:          f.shutdownTimeout,
		// The rate limiter's share counts serving Pods and the hard
		// budget's margin counts every gateway Pod (gatewayReplicas says
		// why).
		RateLimitReplicas: func() int { return replicas().serving },
		BudgetReplicas:    func() int { return replicas().all },
	}, store, tokens, gateway.NewMemorySpend())
	server.Async = async
	server.Completions = &gateway.KubeCompletionWriter{Client: clientset}
	server.Metrics = gateway.NewMetrics(metrics.Registry)
	server.Recorder = eventrec.Bounded(cl.GetEventRecorder("kaalm-gateway"))
	server.Informers = requestPathInformers(cl.GetCache(), logger)

	// Prometheus metrics on a dedicated unauthenticated in-cluster port.
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))
		srv := &http.Server{Addr: f.metricsAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("metrics listener failed", "error", err)
		}
	}()
	// Profiling on its own port, only when asked for (debug only). The
	// listener is unauthenticated and meant for a port-forward during a
	// profiling session, never for a Service.
	profiling.Start(f.pprofAddr, func(err error) { logger.Error("pprof listener failed", "error", err) })
	if activatorClient, err := gateway.NewControllerActivator(
		operatorNamespace, f.certFile, f.keyFile, f.caFile); err == nil {
		server.Activator = activatorClient
	} else {
		logger.Info("activator client disabled", "reason", err.Error())
	}

	// Two contexts: the signal only starts the drain (sigCtx), while
	// everything the request path depends on (the cache, Secret watches, the
	// budget exchange, tracing) runs on runCtx until the drain has finished.
	// Stopping them at the signal would break the requests the drain serves.
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
	store.Secrets = secretwatch.New(runCtx, clientset)
	// A Secret the gateway may not read yet answers its last refusal for a
	// second instead of costing a GET per request, and a grant that lands is
	// used within that second.
	store.Secrets.ErrorTTL = time.Second

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

	// The budget counter exchange: publish this replica's partials and fold
	// peers' on a 10s cadence. POD_NAME comes from the downward API.
	podName := os.Getenv("POD_NAME")
	if podName == "" {
		podName, _ = os.Hostname()
	}
	defer setupTracing(runCtx, server, f.otlpEndpoint, f.otlpSampleRatio, f.upstreamCAFile, podName, logger)()
	publisher := &gateway.BudgetPublisher{
		Client: clientset, Ledger: server.Budget,
		OperatorNamespace: operatorNamespace, PodName: podName,
		Providers: func(ctx context.Context) []*kaalmv1beta1.ModelProvider {
			var list kaalmv1beta1.ModelProviderList
			if err := cl.GetClient().List(ctx, &list); err != nil {
				return nil
			}
			out := make([]*kaalmv1beta1.ModelProvider, 0, len(list.Items))
			for i := range list.Items {
				out = append(out, &list.Items[i])
			}
			return out
		},
	}
	publisher.SeedFromCanonical(runCtx)
	// The publisher stops after the drain, before the cache: its last
	// publish lists the providers through the cache.
	publishCtx, stopPublisher := context.WithCancel(runCtx)
	publisherDone := make(chan struct{})
	go func() {
		defer close(publisherDone)
		publisher.Run(publishCtx)
	}()
	if err := metrics.Registry.Register(&gateway.BudgetUtilizationCollector{
		Ledger: server.Budget, Providers: publisher.Providers,
	}); err != nil {
		logger.Error("registering the budget utilization gauge failed", "error", err)
		os.Exit(1)
	}

	// The watch-driven half of the budget fold: peer partials land in the
	// enforcement view one watch propagation after they are published, which
	// hard enforcement's boundary region depends on (the tick remains the
	// backstop). The cache scopes ConfigMaps to the operator namespace above.
	if informer, err := cl.GetCache().GetInformer(runCtx, &corev1.ConfigMap{}); err == nil {
		_, _ = informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
			AddFunc: func(obj any) {
				gateway.FoldBudgetConfigMapEvent(runCtx, obj, podName, store, server.Budget)
			},
			UpdateFunc: func(_, newObj any) {
				gateway.FoldBudgetConfigMapEvent(runCtx, newObj, podName, store, server.Budget)
			},
		})
	} else {
		logger.Warn("budget ConfigMap informer unavailable; folds ride the tick only", "error", err)
	}

	// The gateway's half of the channel-delete handshake (Add and Update).
	if informer, err := cl.GetCache().GetInformer(runCtx, &kaalmv1beta1.AgentChannel{}); err == nil {
		_, _ = informer.AddEventHandler(channelDisconnectHandler(runCtx, cl.GetClient(), logger))
	}

	logger.Info("kaalm gateway starting",
		"listen", f.listenAddr, "health", f.healthAddr, "operator_namespace", operatorNamespace,
		"source_ip_check_disabled", f.disableSourceIPCheck)
	if err := server.Run(sigCtx); err != nil {
		logger.Error("gateway listener failed", "error", err)
		os.Exit(1)
	}
	stopPublisher()
	<-publisherDone
	cancelRun()
	logger.Info("kaalm gateway shut down")
}

// requestPathInformers registers the informers the request path reads,
// before the cache starts, so the startup WaitForCacheSync covers every one
// of them and /readyz can report each by name. Registered lazily, an
// informer would first sync inside a live request. The list is every kind
// KubeStore reads from the cache, and matches the table in
// docs/src/gateways/llm/operations.md#gateway-readiness.
func requestPathInformers(c cache.Cache, logger *slog.Logger) []gateway.InformerSync {
	objects := []struct {
		name string
		obj  client.Object
	}{
		{"Pod", &corev1.Pod{}},
		{"Agent", &kaalmv1beta1.Agent{}},
		{"AgentTask", &kaalmv1beta1.AgentTask{}},
		{"AgentClass", &kaalmv1beta1.AgentClass{}},
		{"AgentChannel", &kaalmv1beta1.AgentChannel{}},
		{"ModelProvider", &kaalmv1beta1.ModelProvider{}},
		{"ToolProvider", &kaalmv1beta1.ToolProvider{}},
	}
	out := make([]gateway.InformerSync, 0, len(objects))
	for _, o := range objects {
		informer, err := c.GetInformer(context.Background(), o.obj, cache.BlockUntilSynced(false))
		if err != nil {
			logger.Error("registering informer", "kind", o.name, "error", err)
			os.Exit(1)
		}
		out = append(out, gateway.InformerSync{Name: o.name, HasSynced: informer.HasSynced})
	}
	return out
}

// parseBackoff parses a comma-separated duration schedule like "1s,5s,25s".
// An empty or malformed value falls back to the Config default (nil).
// splitPaths parses a comma-separated file list, dropping blanks so an unset
// flag yields no paths.
func splitPaths(csv string) []string {
	var out []string
	for _, part := range strings.Split(csv, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

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

func parseBackoff(raw string) ([]time.Duration, error) {
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]time.Duration, 0, len(parts))
	for _, part := range parts {
		d, err := time.ParseDuration(strings.TrimSpace(part))
		if err != nil {
			// A skipped entry would silently shorten the retry schedule;
			// failing startup keeps the schedule exactly what the operator
			// wrote.
			return nil, fmt.Errorf("malformed backoff entry %q: %w", part, err)
		}
		out = append(out, d)
	}
	return out, nil
}

// parseStartupPolicy applies the strict-parse contract to the callback
// allowlist and the platform base URLs: a malformed value fails startup
// instead of half-applying. These other Helm-derived flags get the same
// treatment elsewhere: the CA bundle paths in validateCABundle, the backoff
// schedules in mustParseBackoff, the size, count, and timeout flags in
// validateLimits, and otlpEndpoint inside NewTracing, which setupTracing
// calls after the cache syncs.
func parseStartupPolicy(
	callbackAllowlist, discordBaseURL, whatsAppBaseURL string, logger *slog.Logger,
) callbackpolicy.Policy {
	policy, err := callbackpolicy.NewFromCSVStrict(callbackAllowlist)
	if err != nil {
		logger.Error("parsing --callback-url-allowlist", "error", err)
		os.Exit(1)
	}
	for flagName, value := range map[string]string{
		"platform-discord-api-base-url":  discordBaseURL,
		"platform-whatsapp-api-base-url": whatsAppBaseURL,
	} {
		if err := validatePlatformBaseURL(value); err != nil {
			logger.Error("malformed platform base URL", "flag", flagName, "error", err)
			os.Exit(1)
		}
	}
	return policy
}

// validateLimits rejects a negative size, count, or timeout flag. Zero is
// valid for every one of these flags and keeps its documented default or
// fallback, while a negative value has no meaning and would otherwise
// half-apply: a 0-byte cap, an already-expired deadline, or a quiet switch
// to another value. The keys are flag names without the leading dashes; the
// error names every negative flag, in a fixed order, so one run reports them
// all.
func validateLimits(counts map[string]int64, durations map[string]time.Duration) error {
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(counts)) {
		if v := counts[name]; v < 0 {
			errs = append(errs, fmt.Errorf("--%s must not be negative, got %d", name, v))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(durations)) {
		if v := durations[name]; v < 0 {
			errs = append(errs, fmt.Errorf("--%s must not be negative, got %s", name, v))
		}
	}
	return errors.Join(errs...)
}

// limitFlags builds validateLimits' input from the parsed flags: every size,
// count, and timeout flag where zero means the default or fallback, keyed by
// flag name.
func limitFlags(f *gatewayFlags) (map[string]int64, map[string]time.Duration) {
	return map[string]int64{
		"max-llm-body-bytes":      f.maxBodyBytes,
		"max-message-body-bytes":  f.maxMessageBodyBytes,
		"max-response-body-bytes": f.maxResponseBodyBytes,
		"max-fallback-depth":      int64(f.maxFallbackDepth),
		"mcp-max-body-bytes":      f.mcpMaxBodyBytes,
	}, map[string]time.Duration{
		"upstream-timeout":       f.upstreamTimeout,
		"sync-delivery-deadline": f.syncDeliveryDeadline,
		"agent-read-timeout":     f.agentReadTimeout,
		"callback-read-timeout":  f.callbackReadTimeout,
		"agent-connect-timeout":  f.agentConnectTimeout,
		"channel-health-window":  f.channelHealthWindow,
		"mcp-upstream-timeout":   f.mcpUpstreamTimeout,
	}
}

// mustParseBackoff is parseBackoff with the startup-failure contract.
func mustParseBackoff(raw, flagName string, logger *slog.Logger) []time.Duration {
	out, err := parseBackoff(raw)
	if err != nil {
		logger.Error("parsing backoff schedule", "flag", flagName, "error", err)
		os.Exit(1)
	}
	return out
}

// validateMaxWakeTimeout rejects a cap that is not positive. Zero is not
// "no cap": an operator who sets it expecting the cap to go away finds out at
// startup instead of getting the default.
func validateMaxWakeTimeout(d time.Duration) error {
	if d <= 0 {
		return fmt.Errorf("max wake timeout must be positive, got %v", d)
	}
	return nil
}

// validatePlatformBaseURL rejects a malformed platform base URL: reply URLs
// are built by appending path segments to it, so it must be
// http(s)://host[:port][/path] with no user info, query, or fragment, and a
// bad value would otherwise surface only as delivery failures at runtime.
// The error never echoes the value raw, so a credential in it stays out of
// the log.
func validatePlatformBaseURL(value string) error {
	_, err := gateway.ParseBaseURL("platform base URL", value)
	return err
}

// setupTracing wires the OTLP exporter when an endpoint is configured and
// returns the shutdown flush; the empty default leaves Server.Tracing nil
// (no tracer installed, no trace context created or forwarded).
func setupTracing(ctx context.Context, server *gateway.Server,
	endpoint string, sampleRatio float64, upstreamCAFile, podName string, logger *slog.Logger,
) func() {
	if endpoint == "" {
		return func() {}
	}
	pool, err := (&tlsutil.CAPoolLoader{Files: splitPaths(upstreamCAFile), Additive: true}).Load()
	if err != nil {
		logger.Error("building the tracing trust pool failed", "error", err)
		os.Exit(1)
	}
	tracing, err := gateway.NewTracing(ctx, endpoint, sampleRatio, pool, podName)
	if err != nil {
		logger.Error("initializing tracing failed", "error", err)
		os.Exit(1)
	}
	server.Tracing = tracing
	logger.Info("tracing enabled", "endpoint", endpoint, "sampleRatio", sampleRatio)
	return func() { _ = tracing.Shutdown(context.Background()) }
}

// clusterOptions configures the gateway's shared cache and client.
func clusterOptions(scheme *runtime.Scheme, operatorNamespace string) func(*cluster.Options) {
	return func(o *cluster.Options) {
		o.Scheme = scheme
		// Secrets are never read through the shared informer cache. The
		// gateway holds only get/watch on Secrets in kaalm-system plus
		// dynamic resourceNames-scoped grants on individual channel
		// Secrets (no cluster-wide list), so a cached Secret informer would
		// issue a forbidden cluster-scoped LIST and the read would hang
		// waiting for a sync that never lands. Secret reads go through
		// secretwatch.Watcher instead: one GET-backed, name-filtered watch
		// per referenced Secret. See
		// docs/src/security/rbac.md#namespaced-grants-in-kaalm-system.
		o.Client.Cache = &client.CacheOptions{
			DisableFor: []client.Object{&corev1.Secret{}},
		}
		// Nothing in the gateway reads managedFields, and on a Pod they
		// are the largest single field; dropping them at the informer
		// shrinks every cached object and every copy made of one.
		o.Cache.DefaultTransform = cache.TransformStripManagedFields()
		noDeepCopy := true
		readOnly := cache.ByObject{UnsafeDisableDeepCopy: &noDeepCopy}
		// The ConfigMap informer (budget watch-fold) must be scoped to the
		// operator namespace: the gateway's Role grants list/watch there
		// only, and an unscoped informer would issue a forbidden
		// cluster-wide LIST and hang, the same failure mode the Secret
		// comment above describes.
		o.Cache.ByObject = map[client.Object]cache.ByObject{
			&corev1.ConfigMap{}: {Namespaces: map[string]cache.Config{
				operatorNamespace: {},
			}},
			// The request paths Get each of these kinds once or more per
			// request, and a deep copy per Get was a tenth of the gateway's
			// allocations. No gateway code changes these objects; a reader
			// that must, deep copies first (the async and platform intake
			// do before handing objects to goroutines). The budget
			// publisher's ModelProvider List shares memory the same way.
			&kaalmv1beta1.Agent{}:         readOnly,
			&kaalmv1beta1.AgentTask{}:     readOnly,
			&kaalmv1beta1.AgentClass{}:    readOnly,
			&kaalmv1beta1.ModelProvider{}: readOnly,
			&kaalmv1beta1.ToolProvider{}:  readOnly,
		}
	}
}
