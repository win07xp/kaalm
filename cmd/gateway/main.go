// Command gateway is the Agentry Gateway: the LLM listener on :8443 with
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
	"syscall"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
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

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
	"github.com/win07xp/kubeclaw/internal/gateway"
)

func main() {
	var (
		listenAddr           string
		healthAddr           string
		certFile, keyFile    string
		caFile               string
		upstreamCAFile       string
		maxBodyBytes         int64
		upstreamTimeout      time.Duration
		disableSourceIPCheck bool
		userAddr             string
		agentHostOverride    string
		agentPortOverride    int
	)
	flag.StringVar(&listenAddr, "listen-addr", ":8443", "LLM listener address")
	flag.StringVar(&healthAddr, "health-addr", ":8081", "health listener address")
	flag.StringVar(&certFile, "tls-cert", "/var/run/agentry/tls.crt", "serving certificate file")
	flag.StringVar(&keyFile, "tls-key", "/var/run/agentry/tls.key", "serving key file")
	flag.StringVar(&caFile, "tls-ca", "/var/run/agentry/ca.crt", "Agentry CA bundle for client verification")
	flag.StringVar(&upstreamCAFile, "upstream-ca", "", "optional extra CA bundle for upstream provider TLS")
	flag.Int64Var(&maxBodyBytes, "max-llm-body-bytes", 4<<20, "inbound LLM request body cap")
	flag.DurationVar(&upstreamTimeout, "upstream-timeout", 120*time.Second, "upstream provider call timeout")
	flag.BoolVar(&disableSourceIPCheck, "disable-source-ip-check", false,
		"skip the source-IP-to-Pod cross-check (dev only; the check is defense in depth and must stay on in-cluster)")
	flag.StringVar(&userAddr, "user-addr", ":8080", "User Gateway listener address")
	flag.StringVar(&agentHostOverride, "agent-host-override", "", "redirect agent delivery dials to this host (dev only)")
	flag.IntVar(&agentPortOverride, "agent-port-override", 0, "redirect agent delivery dials to this port (dev only)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	operatorNamespace := os.Getenv("POD_NAMESPACE")
	if operatorNamespace == "" {
		operatorNamespace = "agentry-system"
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(agentryv1alpha1.AddToScheme(scheme))
	utilruntime.Must(cmapi.AddToScheme(scheme))

	restCfg := ctrl.GetConfigOrDie()
	cl, err := cluster.New(restCfg, func(o *cluster.Options) { o.Scheme = scheme })
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

	var upstreamCAs *x509.CertPool
	if upstreamCAFile != "" {
		pem, err := os.ReadFile(upstreamCAFile)
		if err != nil {
			logger.Error("reading upstream CA bundle", "error", err)
			os.Exit(1)
		}
		upstreamCAs = x509.NewCertPool()
		if !upstreamCAs.AppendCertsFromPEM(pem) {
			logger.Error("no certificates parsed from upstream CA bundle")
			os.Exit(1)
		}
	}

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
		UpstreamCAs:              upstreamCAs,
		DisableSourceIPCheck:     disableSourceIPCheck,
		UserListenAddr:           userAddr,
		AgentServiceHostOverride: agentHostOverride,
		AgentServicePortOverride: int32(agentPortOverride),
	}, store, tokens, gateway.NewMemorySpend())
	server.Async = async
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
		Providers: func(ctx context.Context) []*agentryv1alpha1.ModelProvider {
			var list agentryv1alpha1.ModelProviderList
			if err := cl.GetClient().List(ctx, &list); err != nil {
				return nil
			}
			out := make([]*agentryv1alpha1.ModelProvider, 0, len(list.Items))
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
	if informer, err := cl.GetCache().GetInformer(ctx, &agentryv1alpha1.AgentChannel{}); err == nil {
		_, _ = informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
			UpdateFunc: func(_, newObj any) {
				ch, ok := newObj.(*agentryv1alpha1.AgentChannel)
				if !ok || ch.Status.Phase != agentryv1alpha1.ChannelTerminating {
					return
				}
				if ch.Annotations[agentryv1alpha1.AnnotationChannelDisconnected] == agentryv1alpha1.AnnotationTrue {
					return
				}
				patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`,
					agentryv1alpha1.AnnotationChannelDisconnected, agentryv1alpha1.AnnotationTrue))
				if err := cl.GetClient().Patch(ctx, ch.DeepCopy(),
					client.RawPatch(types.MergePatchType, patch)); err != nil {
					logger.Warn("disconnect annotation patch failed", "channel", ch.Name, "error", err)
				}
			},
		})
	}

	logger.Info("agentry gateway starting",
		"listen", listenAddr, "health", healthAddr, "operator_namespace", operatorNamespace,
		"source_ip_check_disabled", disableSourceIPCheck)
	if err := server.Run(ctx); err != nil {
		logger.Error("gateway listener failed", "error", err)
		os.Exit(1)
	}
	logger.Info("agentry gateway shut down")
}
