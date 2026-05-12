package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cartridge-gg/discordgo"
	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/audio"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/channels/discord/voice"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// VoiceWorkerConfig wires one standalone voice transcription worker. The
// worker owns its Discord session and exits after the supervised voice session
// ends, or after StartupGrace if no call is active.
type VoiceWorkerConfig struct {
	ChannelInstanceName string
	ChannelInstances    store.ChannelInstanceStore
	AgentStore          store.AgentStore
	MemoryStore         store.MemoryStore
	AudioManager        *audio.Manager
	ProviderRegistry    *providers.Registry
	VoiceSummarizer     *config.VoiceSummarizerConfig
	SkillBodyLoader     func(name string) (string, error)
	StartupGrace        time.Duration
}

// RunVoiceWorker runs the real-time Discord voice supervisor in a standalone
// process for exactly one channel instance.
func RunVoiceWorker(ctx context.Context, cfg VoiceWorkerConfig) error {
	if cfg.ChannelInstances == nil {
		return errors.New("voice worker: channel instance store is required")
	}
	if cfg.AudioManager == nil {
		return errors.New("voice worker: audio manager is required")
	}
	name := cfg.ChannelInstanceName
	if name == "" {
		name = os.Getenv("GOCLAW_VOICE_CHANNEL_INSTANCE")
	}
	if name == "" {
		return errors.New("voice worker: --channel-instance is required")
	}
	startupGrace := cfg.StartupGrace
	if startupGrace <= 0 {
		startupGrace = 2 * time.Minute
	}

	inst, err := cfg.ChannelInstances.GetByName(store.WithCrossTenant(ctx), name)
	if err != nil {
		return fmt.Errorf("voice worker: load channel instance %q: %w", name, err)
	}
	if inst == nil {
		return fmt.Errorf("voice worker: channel instance %q not found", name)
	}
	if !inst.Enabled {
		return fmt.Errorf("voice worker: channel instance %q is disabled", name)
	}

	var creds discordCreds
	if len(inst.Credentials) > 0 {
		if err := json.Unmarshal(inst.Credentials, &creds); err != nil {
			return fmt.Errorf("voice worker: decode discord credentials: %w", err)
		}
	}
	if creds.Token == "" {
		return errors.New("voice worker: discord token is required")
	}

	var ic discordInstanceConfig
	if len(inst.Config) > 0 {
		if err := json.Unmarshal(inst.Config, &ic); err != nil {
			return fmt.Errorf("voice worker: decode discord config: %w", err)
		}
	}
	if ic.VoiceChannelEnabled == nil || !*ic.VoiceChannelEnabled {
		return fmt.Errorf("voice worker: channel instance %q has voice disabled", name)
	}

	session, err := discordgo.New("Bot " + creds.Token)
	if err != nil {
		return fmt.Errorf("voice worker: create discord session: %w", err)
	}
	session.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildVoiceStates
	if err := session.Open(); err != nil {
		return fmt.Errorf("voice worker: open discord session: %w", err)
	}
	defer session.Close()

	bot, err := session.User("@me")
	if err != nil {
		return fmt.Errorf("voice worker: fetch bot identity: %w", err)
	}

	vcfg := voice.Config{
		VoiceChannelID:      ic.VoiceChannelID,
		TranscriptChannelID: ic.VoiceChannelTranscriptChannelID,
		IdleLeaveSeconds:    ic.VoiceChannelIdleLeaveSeconds,
		MinUtteranceMs:      ic.VoiceChannelMinUtteranceMs,
		MaxUtteranceMs:      ic.VoiceChannelMaxUtteranceMs,
		DailyCapSeconds:     ic.VoiceChannelDailyCapSeconds,
		StopAfterSession:    true,
	}
	if summarizer := buildWorkerVoiceSummarizer(ctx, cfg, inst); summarizer != nil {
		vcfg.TranscriptSummarizer = summarizer
	}

	joined := make(chan struct{})
	var joinOnce sync.Once
	done := make(chan string, 1)
	vcfg.OnJoin = func() {
		joinOnce.Do(func() { close(joined) })
	}
	vcfg.OnLeave = func(reason string) {
		select {
		case done <- reason:
		default:
		}
	}

	voiceTmp := filepath.Join("/data", "voice-tmp")
	if err := os.MkdirAll(voiceTmp, 0o755); err != nil {
		slog.Warn("voice worker: tmp dir create failed; falling back to os tmp",
			"err", err, "preferred", voiceTmp)
		voiceTmp = voice.DefaultTmpDir()
	}

	sup, err := voice.NewSupervisor(vcfg, session, cfg.AudioManager, voiceTmp, bot.ID, slog.Default())
	if err != nil {
		return fmt.Errorf("voice worker: create supervisor: %w", err)
	}
	sup.Start(ctx)
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		sup.Stop(stopCtx)
		cancel()
	}()

	startupTimer := time.NewTimer(startupGrace)
	defer startupTimer.Stop()
	select {
	case <-joined:
		slog.Info("voice worker: joined voice channel", "channel_instance", name, "voice_channel_id", ic.VoiceChannelID)
	case reason := <-done:
		slog.Info("voice worker: voice session ended before join callback", "reason", reason)
		return nil
	case <-startupTimer.C:
		slog.Info("voice worker: no active voice session before startup grace elapsed", "startup_grace", startupGrace)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case reason := <-done:
		slog.Info("voice worker: voice session ended", "reason", reason)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func buildWorkerVoiceSummarizer(ctx context.Context, cfg VoiceWorkerConfig, inst *store.ChannelInstanceData) voice.TranscriptSummarizer {
	if cfg.ProviderRegistry == nil || cfg.AgentStore == nil {
		return nil
	}
	ag, err := cfg.AgentStore.GetByIDUnscoped(ctx, inst.AgentID)
	if err != nil || ag == nil {
		slog.Warn("voice worker: cannot load agent for summarizer", "agent_id", inst.AgentID, "err", err)
		return nil
	}

	tctx := store.WithTenantID(ctx, inst.TenantID)
	providerName := ag.Provider
	model := ag.Model
	maxTokens := 0
	thinkingLevel := ""
	sessionOutputDir := ""
	skillName := ""
	if cfg.VoiceSummarizer != nil {
		if cfg.VoiceSummarizer.Provider != "" {
			providerName = cfg.VoiceSummarizer.Provider
			model = cfg.VoiceSummarizer.Model
		}
		maxTokens = cfg.VoiceSummarizer.MaxTokens
		thinkingLevel = cfg.VoiceSummarizer.ThinkingLevel
		sessionOutputDir = cfg.VoiceSummarizer.SessionOutputDir
		skillName = cfg.VoiceSummarizer.SkillName
	}
	if providerName == "" {
		return nil
	}
	p, err := cfg.ProviderRegistry.Get(tctx, providerName)
	if err != nil {
		slog.Warn("voice worker: summarizer provider not found", "provider", providerName, "err", err)
		return nil
	}
	if model == "" {
		model = p.DefaultModel()
	}

	skillBody := ""
	if skillName != "" && cfg.SkillBodyLoader != nil {
		if body, err := cfg.SkillBodyLoader(skillName); err == nil {
			skillBody = body
		} else {
			slog.Warn("voice worker: voice_summarizer skill not found", "skill", skillName, "err", err)
		}
	}

	var memAdapter channels.MemoryQueryer
	if cfg.MemoryStore != nil && ag.ID != uuid.Nil {
		memAdapter = channels.NewVoiceMemoryQueryer(cfg.MemoryStore)
	}
	return channels.BuildVoiceTranscriptSummarizer(&channels.VoiceTranscriptSummarizerConfig{
		Provider:         p,
		Model:            model,
		MaxOutputTokens:  maxTokens,
		ThinkingLevel:    thinkingLevel,
		SkillBody:        skillBody,
		MemoryStore:      memAdapter,
		MemoryAgentID:    ag.ID.String(),
		SessionOutputDir: sessionOutputDir,
		MemoryWorkspace:  ag.Workspace,
	})
}
