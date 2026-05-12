package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/cartridge-gg/discordgo"
	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/jobs"
	"github.com/nextlevelbuilder/goclaw/internal/safego"
)

const (
	voiceJobEnsureInterval = 30 * time.Second
	voiceJobEnsureDebounce = 5 * time.Second
	voiceJobKickCooldown   = 5 * time.Minute
)

type voiceJobCoordinatorConfig struct {
	channelName string
	session     *discordgo.Session
	jobSvc      *jobs.Service
	discordCfg  config.DiscordConfig
	botUserID   string
	log         *slog.Logger
}

// voiceJobCoordinator is the lightweight parent-process watcher for
// voice_channel_runner=job. It never joins voice itself; it only watches human
// presence and asks the agent service to ensure a deduped voice-worker Job.
type voiceJobCoordinator struct {
	channelName string
	session     *discordgo.Session
	jobSvc      *jobs.Service
	discordCfg  config.DiscordConfig
	botUserID   string
	log         *slog.Logger

	resolvedGuildID string
	humans          map[string]struct{}
	removers        []func()

	mu             sync.Mutex
	wg             sync.WaitGroup
	stopCh         chan struct{}
	stopOnce       sync.Once
	nowFn          func() time.Time
	lastEnsure     time.Time
	ensureInFlight bool
	kickedUntil    time.Time
}

func newVoiceJobCoordinator(cfg voiceJobCoordinatorConfig) (*voiceJobCoordinator, error) {
	if cfg.channelName == "" {
		return nil, errors.New("voice job coordinator: channel name is required")
	}
	if cfg.session == nil {
		return nil, errors.New("voice job coordinator: nil discord session")
	}
	if cfg.jobSvc == nil {
		return nil, errors.New("voice job coordinator: nil jobs service")
	}
	if cfg.discordCfg.VoiceChannelID == "" {
		return nil, errors.New("voice job coordinator: voice channel id is required")
	}
	if cfg.discordCfg.VoiceChannelTranscriptChannelID == "" {
		return nil, errors.New("voice job coordinator: transcript channel id is required")
	}
	log := cfg.log
	if log == nil {
		log = slog.Default()
	}
	return &voiceJobCoordinator{
		channelName: cfg.channelName,
		session:     cfg.session,
		jobSvc:      cfg.jobSvc,
		discordCfg:  cfg.discordCfg,
		botUserID:   cfg.botUserID,
		log: log.With(
			"component", "discord.voice_job_coordinator",
			"channel", cfg.channelName,
			"voice_channel_id", cfg.discordCfg.VoiceChannelID,
		),
		humans: make(map[string]struct{}),
		stopCh: make(chan struct{}),
		nowFn:  time.Now,
	}, nil
}

func (c *voiceJobCoordinator) Start(ctx context.Context) {
	resolveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	ch, err := c.session.Channel(c.discordCfg.VoiceChannelID, discordgo.WithContext(resolveCtx))
	cancel()
	if err != nil || ch == nil {
		c.log.Error("voice job coordinator: cannot resolve guild for voice channel; coordinator will no-op",
			"err", err, "voice_channel_id", c.discordCfg.VoiceChannelID)
	} else {
		c.resolvedGuildID = ch.GuildID
		c.log = c.log.With("guild_id", c.resolvedGuildID)
	}

	c.removers = append(c.removers,
		c.session.AddHandler(c.onVoiceStateUpdate),
		c.session.AddHandler(c.onGuildCreate),
	)
	c.primeFromState()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer safego.Recover(nil, "component", "discord.voice_job_coordinator.loop")
		ticker := time.NewTicker(voiceJobEnsureInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c.mu.Lock()
				c.reconcileLocked()
				c.mu.Unlock()
			case <-ctx.Done():
				c.stopOnce.Do(func() { close(c.stopCh) })
				return
			case <-c.stopCh:
				return
			}
		}
	}()
}

func (c *voiceJobCoordinator) Stop(ctx context.Context) {
	c.stopOnce.Do(func() { close(c.stopCh) })

	c.mu.Lock()
	removers := c.removers
	c.removers = nil
	c.mu.Unlock()
	for _, rm := range removers {
		if rm != nil {
			rm()
		}
	}

	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		c.log.Warn("voice job coordinator: Stop() wait cancelled", "err", ctx.Err())
	}
}

func (c *voiceJobCoordinator) onVoiceStateUpdate(_ *discordgo.Session, ev *discordgo.VoiceStateUpdate) {
	if ev == nil || ev.VoiceState == nil || c.resolvedGuildID == "" || ev.GuildID != c.resolvedGuildID {
		return
	}
	if ev.UserID == c.botUserID {
		c.onOwnVoiceState(ev)
		return
	}

	inOurChannel := ev.ChannelID == c.discordCfg.VoiceChannelID
	c.mu.Lock()
	defer c.mu.Unlock()
	_, wasPresent := c.humans[ev.UserID]
	switch {
	case inOurChannel && !wasPresent:
		c.humans[ev.UserID] = struct{}{}
	case !inOurChannel && wasPresent:
		delete(c.humans, ev.UserID)
	default:
		return
	}
	c.reconcileLocked()
}

func (c *voiceJobCoordinator) onGuildCreate(_ *discordgo.Session, ev *discordgo.GuildCreate) {
	if ev == nil || ev.Guild == nil || c.resolvedGuildID == "" || ev.Guild.ID != c.resolvedGuildID {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, vs := range ev.Guild.VoiceStates {
		if vs == nil || vs.ChannelID != c.discordCfg.VoiceChannelID || vs.UserID == c.botUserID {
			continue
		}
		c.humans[vs.UserID] = struct{}{}
	}
	c.reconcileLocked()
}

func (c *voiceJobCoordinator) onOwnVoiceState(ev *discordgo.VoiceStateUpdate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.humans) == 0 || ev.ChannelID == c.discordCfg.VoiceChannelID {
		return
	}
	c.kickedUntil = c.nowFn().Add(voiceJobKickCooldown)
	c.log.Warn("voice job coordinator: bot removed from voice channel; cooling off",
		"new_channel_id", ev.ChannelID,
		"cooldown_until", c.kickedUntil.Format(time.RFC3339))
}

func (c *voiceJobCoordinator) primeFromState() {
	if c.session.State == nil || c.resolvedGuildID == "" {
		return
	}
	guild, err := c.session.State.Guild(c.resolvedGuildID)
	if err != nil || guild == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, vs := range guild.VoiceStates {
		if vs == nil || vs.ChannelID != c.discordCfg.VoiceChannelID || vs.UserID == c.botUserID {
			continue
		}
		c.humans[vs.UserID] = struct{}{}
	}
	c.reconcileLocked()
}

func (c *voiceJobCoordinator) reconcileLocked() {
	now := c.nowFn()
	if len(c.humans) == 0 || c.ensureInFlight {
		return
	}
	if !c.kickedUntil.IsZero() && now.Before(c.kickedUntil) {
		return
	}
	if !c.lastEnsure.IsZero() && now.Sub(c.lastEnsure) < voiceJobEnsureDebounce {
		return
	}
	c.ensureInFlight = true
	c.lastEnsure = now

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer safego.Recover(nil, "component", "discord.voice_job_coordinator.ensure")
		c.ensureJob()
		c.mu.Lock()
		c.ensureInFlight = false
		c.mu.Unlock()
	}()
}

func (c *voiceJobCoordinator) ensureJob() {
	req := c.voiceJobRequest()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := c.jobSvc.Spawn(ctx, req)
	if err != nil {
		c.log.Error("voice job coordinator: ensure job failed", "err", err, "dedup_key", req.DedupKey)
		return
	}
	c.log.Info("voice job coordinator: ensured voice transcription job",
		"job_id", resp.JobID,
		"k8s_job_name", resp.K8sJobName,
		"k8s_job_uid", resp.K8sJobUID,
		"dedup_key", req.DedupKey,
	)
}

func (c *voiceJobCoordinator) voiceJobRequest() jobs.Request {
	name := safeVoiceJobName(c.channelName)
	return jobs.Request{
		JobID:         uuid.NewString(),
		Kind:          "voice-transcription",
		Command:       "/usr/local/bin/goclaw-entrypoint",
		Args:          []string{"voice-worker", "--config", "/data/goclaw/config.json", "--channel-instance", c.channelName},
		Cwd:           "/data/goclaw",
		WorkspaceRoot: "/data/goclaw",
		WorktreePath:  fmt.Sprintf("/data/voice-jobs/%s", name),
		DedupKey:      fmt.Sprintf("voice:%s:%s", c.channelName, c.discordCfg.VoiceChannelID),
		Resources: jobs.Resources{
			CPURequest:    "500m",
			CPULimit:      "2",
			MemoryRequest: "512Mi",
			MemoryLimit:   "2Gi",
		},
		Sinks: []jobs.Sink{},
		Env: map[string]string{
			"GOCLAW_VOICE_CHANNEL_INSTANCE": c.channelName,
		},
	}
}

func safeVoiceJobName(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		ok := unicode.IsLetter(r) || unicode.IsDigit(r)
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "voice"
	}
	if len(out) > 48 {
		out = strings.Trim(out[:48], "-")
	}
	if out == "" {
		return "voice"
	}
	return out
}
