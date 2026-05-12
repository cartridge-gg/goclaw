package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/nextlevelbuilder/goclaw/internal/audio"
	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels/discord"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/skills"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

func voiceWorkerCmd() *cobra.Command {
	var channelInstance string
	var startupGrace time.Duration
	cmd := &cobra.Command{
		Use:   "voice-worker",
		Short: "Run one Discord voice transcription worker",
		Run: func(cmd *cobra.Command, args []string) {
			runVoiceWorker(channelInstance, startupGrace)
		},
	}
	cmd.Flags().StringVar(&channelInstance, "channel-instance", "", "channel instance name to supervise")
	cmd.Flags().DurationVar(&startupGrace, "startup-grace", 2*time.Minute, "exit if no active voice session starts within this duration")
	return cmd
}

func runVoiceWorker(channelInstance string, startupGrace time.Duration) {
	setupVoiceWorkerLogging()

	cfg, err := config.Load(resolveConfigPath())
	if err != nil {
		slog.Error("voice worker: failed to load config", "error", err)
		os.Exit(1)
	}
	dataDir := cfg.ResolvedDataDir()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		slog.Error("voice worker: failed to create data dir", "error", err, "data_dir", dataDir)
		os.Exit(1)
	}

	modelReg := providers.NewInMemoryRegistry()
	modelReg.RegisterResolver("anthropic", &providers.AnthropicForwardCompat{})
	modelReg.RegisterResolver("openai", &providers.OpenAIForwardCompat{})

	providerRegistry := providers.NewRegistry(store.TenantIDFromContext)
	registerProviders(providerRegistry, cfg, modelReg)

	audioMgr := setupTTS(cfg)
	if audioMgr == nil {
		slog.Error("voice worker: no audio manager available")
		os.Exit(1)
	}
	setupAudioExtras(cfg, audioMgr)
	audio.BridgeLegacySTT(audioMgr, cfg)

	msgBus := bus.New()
	pgStores, traceCollector, snapshotWorker := setupStoresAndTracing(cfg, dataDir, msgBus)
	if traceCollector != nil {
		defer traceCollector.Stop()
	}
	if snapshotWorker != nil {
		defer snapshotWorker.Stop()
	}
	if pgStores.ChannelInstances == nil || pgStores.Agents == nil {
		slog.Error("voice worker: required stores are unavailable")
		os.Exit(1)
	}
	if pgStores.Providers != nil {
		registerProvidersFromDB(providerRegistry, pgStores.Providers, pgStores.ConfigSecrets, loopbackAddr(cfg.Gateway.Host, cfg.Gateway.Port), cfg.Gateway.Token, pgStores.MCP, cfg, modelReg)
	}

	workspace := config.ExpandHome(cfg.Agents.Defaults.Workspace)
	if !filepath.IsAbs(workspace) {
		workspace, _ = filepath.Abs(workspace)
	}
	skillsLoader := skills.NewLoader(workspace, voiceWorkerGlobalSkillsDir(dataDir), voiceWorkerBuiltinSkillsDir())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := discord.RunVoiceWorker(ctx, discord.VoiceWorkerConfig{
		ChannelInstanceName: channelInstance,
		ChannelInstances:    pgStores.ChannelInstances,
		AgentStore:          pgStores.Agents,
		MemoryStore:         pgStores.Memory,
		AudioManager:        audioMgr,
		ProviderRegistry:    providerRegistry,
		VoiceSummarizer:     cfg.Channels.VoiceSummarizer,
		SkillBodyLoader: func(name string) (string, error) {
			body, ok := skillsLoader.LoadSkill(context.Background(), name)
			if !ok {
				return "", fmt.Errorf("skill %q not found", name)
			}
			return body, nil
		},
		StartupGrace: startupGrace,
	}); err != nil {
		slog.Error("voice worker failed", "error", err)
		os.Exit(1)
	}
}

func setupVoiceWorkerLogging() {
	logLevel := slog.LevelInfo
	if verbose {
		logLevel = slog.LevelDebug
	}
	if lvl := os.Getenv("GOCLAW_LOG_LEVEL"); lvl != "" {
		switch strings.ToLower(lvl) {
		case "debug":
			logLevel = slog.LevelDebug
		case "info":
			logLevel = slog.LevelInfo
		case "warn":
			logLevel = slog.LevelWarn
		case "error":
			logLevel = slog.LevelError
		default:
			fmt.Fprintf(os.Stderr, "warning: unknown GOCLAW_LOG_LEVEL=%q, using info\n", lvl)
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})))
}

func voiceWorkerGlobalSkillsDir(dataDir string) string {
	if v := os.Getenv("GOCLAW_SKILLS_DIR"); v != "" {
		return v
	}
	return filepath.Join(dataDir, "skills")
}

func voiceWorkerBuiltinSkillsDir() string {
	if v := os.Getenv("GOCLAW_BUILTIN_SKILLS_DIR"); v != "" {
		return v
	}
	return "/app/bundled-skills"
}
