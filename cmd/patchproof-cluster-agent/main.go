package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/patchproof/patchproof/internal/adapters/executors/kubernetes"
	"github.com/patchproof/patchproof/internal/bootstrap"
	"github.com/patchproof/patchproof/internal/clusteragent"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/platform/config"
	"github.com/patchproof/patchproof/internal/platform/logging"
)

func isolationTiers(cfg config.Config) []domain.IsolationTier {
	if len(cfg.Execution.SupportedIsolationTiers) > 0 {
		out := make([]domain.IsolationTier, 0, len(cfg.Execution.SupportedIsolationTiers))
		for _, tier := range cfg.Execution.SupportedIsolationTiers {
			out = append(out, domain.IsolationTier(tier))
		}
		return out
	}
	tiers := []domain.IsolationTier{domain.IsolationTrustedLocal, domain.IsolationContainer}
	if cfg.Execution.RuntimeClass != "" {
		tiers = append(tiers, domain.IsolationGVisor)
	}
	return tiers
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "patchproof-cluster-agent:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv("PATCHPROOF_CONFIG"))
	if err != nil {
		return err
	}
	logger, err := logging.New(cfg.Telemetry, cfg.Service)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if cfg.Execution.ClusterID == "" {
		return fmt.Errorf("PATCHPROOF_CLUSTER_ID is required")
	}
	if cfg.Execution.AgentToken == "" {
		return fmt.Errorf("PATCHPROOF_AGENT_TOKEN is required")
	}
	store, err := bootstrap.OpenStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer store.Close()
	executor, err := kubernetes.New(kubernetes.Config{
		ClusterID: domain.ClusterID(cfg.Execution.ClusterID), Region: cfg.Execution.ClusterRegion,
		Namespace: cfg.Execution.Namespace, Kubeconfig: cfg.Execution.Kubeconfig,
		ServiceAccount: cfg.Execution.ServiceAccount, WorkerImage: cfg.Execution.WorkerImage,
		EngineImage: cfg.Execution.EngineImage, BrokerImage: cfg.Execution.BrokerImage,
		BrokerServiceAccount: cfg.Execution.BrokerServiceAccount,
		BrokeredStorageClass: cfg.Execution.BrokeredStorageClass,
		WorkspaceSizeGiB:     cfg.Execution.WorkspaceSizeGiB, RuntimeClass: cfg.Execution.RuntimeClass,
		JobTTL: cfg.Execution.JobTTL, ActiveDeadline: cfg.Execution.ActiveDeadline,
		MaxConcurrent: cfg.Execution.MaxConcurrent, CallbackURL: cfg.Execution.ReportBackURL,
		ExtraEnv: bootstrap.WorkerEnvironment(cfg), NodeSelector: cfg.Execution.NodeSelector,
		Tolerations:                  bootstrap.Tolerations(cfg.Execution.Tolerations),
		AutomountServiceAccountToken: cfg.Execution.AutomountWorkerToken,
		Languages:                    bootstrap.ConfiguredLanguages(cfg), Engines: bootstrap.ConfiguredEngines(cfg),
	})
	if err != nil {
		return err
	}
	region := cfg.Execution.ClusterRegion
	if region == "" {
		region = cfg.Service.Region
	}
	server, err := clusteragent.New(clusteragent.Config{
		ListenAddr: cfg.HTTP.ListenAddr, Token: cfg.Execution.AgentToken, Executor: executor,
		Registrar: store, HeartbeatInterval: 30 * time.Second, Logger: logger,
		ClusterInfo: domain.Cluster{
			ID: domain.ClusterID(cfg.Execution.ClusterID), Region: region, ExecutorKind: "remote",
			Endpoint:  firstNonEmpty(cfg.Execution.AgentEndpoint, cfg.HTTP.ExternalURL),
			Namespace: cfg.Execution.Namespace, CasRegion: region, Version: cfg.Service.Version,
			Architectures: []string{"amd64"}, Languages: []string{"go"},
			Engines:        []string{"go-analyzer", "go-build", "go-differential", "go-native-fuzz"},
			IsolationTiers: isolationTiers(cfg),
			RuntimeClasses: []string{cfg.Execution.RuntimeClass},
		},
	})
	if err != nil {
		return err
	}
	return server.Start(ctx)
}
