package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/cartridge-gg/discordgo"

	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/channels/discord/voice"
	"github.com/nextlevelbuilder/goclaw/internal/safego"
)

const (
	voiceSummaryRegenThreadFetchLimit = 1000
	voiceSummaryRegenTimeout          = 75 * time.Second
)

var discordMentionRE = regexp.MustCompile(`<@!?([0-9]+)>`)

// tryHandleVoiceSummaryRegenerationRequest lets an operator type "regenerate
// summary" directly in a voice transcript thread. It bypasses normal mention
// gating only after proving the message is inside this channel instance's
// configured transcript channel and after running the normal group policy check
// against that parent transcript channel.
func (c *Channel) tryHandleVoiceSummaryRegenerationRequest(ctx context.Context, m *discordgo.MessageCreate, senderID string) bool {
	if c == nil || c.session == nil || m == nil || m.Message == nil {
		return false
	}
	if !isVoiceSummaryRegenerationRequest(m.Content) {
		return false
	}
	if c.config.VoiceChannelTranscriptChannelID == "" || c.config.VoiceChannelID == "" {
		return false
	}
	thread, ok := c.resolveVoiceTranscriptThread(ctx, m.ChannelID)
	if !ok {
		return false
	}
	if !c.checkGroupPolicy(ctx, senderID, thread.ParentID) {
		return true
	}

	ack, err := c.session.ChannelMessageSend(m.ChannelID, "Regenerating voice summary...")
	if err != nil {
		slog.Warn("discord: voice summary regeneration ack failed", "err", err, "thread_id", m.ChannelID)
	}
	ackID := ""
	if ack != nil {
		ackID = ack.ID
	}

	go func() {
		defer safego.Recover(nil, "component", "discord.voice_summary_regeneration", "thread_id", thread.ID)
		runCtx, cancel := context.WithTimeout(context.Background(), voiceSummaryRegenTimeout)
		defer cancel()
		if err := c.regenerateVoiceSummaryFromThread(runCtx, thread, ackID); err != nil {
			slog.Warn("discord: voice summary regeneration failed", "err", err, "thread_id", thread.ID)
			c.editVoiceSummaryRegenerationAck(context.Background(), thread.ID, ackID, "Voice summary regeneration failed: "+err.Error())
		}
	}()
	return true
}

func isVoiceSummaryRegenerationRequest(content string) bool {
	s := strings.ToLower(strings.TrimSpace(content))
	s = strings.ReplaceAll(s, "_", " ")
	s = strings.ReplaceAll(s, "-", " ")
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return false
	}
	if strings.Contains(s, "summary") || strings.Contains(s, "summar") {
		for _, verb := range []string{"regenerate", "regen", "rerun", "re run", "refresh", "backfill", "redo", "rebuild", "summarize", "summarise"} {
			if strings.Contains(s, verb) {
				return true
			}
		}
	}
	return false
}

func (c *Channel) resolveVoiceTranscriptThread(ctx context.Context, channelID string) (*discordgo.Channel, bool) {
	ch, err := c.session.State.Channel(channelID)
	if err != nil || ch == nil {
		lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		ch, err = c.session.Channel(channelID, discordgo.WithContext(lookupCtx))
		cancel()
		if err != nil || ch == nil {
			return nil, false
		}
	}
	switch ch.Type {
	case discordgo.ChannelTypeGuildPublicThread,
		discordgo.ChannelTypeGuildPrivateThread,
		discordgo.ChannelTypeGuildNewsThread:
	default:
		return nil, false
	}
	if ch.ParentID != c.config.VoiceChannelTranscriptChannelID {
		return nil, false
	}
	return ch, true
}

func (c *Channel) regenerateVoiceSummaryFromThread(ctx context.Context, thread *discordgo.Channel, ackID string) error {
	if c.voiceSummarizerCfg == nil {
		return errors.New("voice transcript summarizer is not configured")
	}
	summarizer := channels.BuildVoiceTranscriptSummarizer(c.voiceSummarizerCfg)
	if summarizer == nil {
		return errors.New("voice transcript summarizer is unavailable")
	}

	parent, err := c.voiceSummaryParentMessage(ctx, thread)
	if err != nil {
		return err
	}
	transcriptMessages, err := c.fetchVoiceTranscriptThreadMessages(ctx, thread.ID)
	if err != nil {
		return err
	}
	transcript, lineTimes, speakerNames := voiceTranscriptFromMessages(transcriptMessages)
	if strings.TrimSpace(transcript) == "" {
		return errors.New("no transcript lines found in this thread")
	}

	startedAt, endedAt := voiceSummaryTimeRange(parent, lineTimes)
	voiceName := c.voiceChannelName(ctx)
	if voiceName == "" {
		voiceName = voiceChannelNameFromSummary(parent.Content)
	}
	if voiceName == "" {
		voiceName = c.config.VoiceChannelID
	}
	speakers := c.voiceSummarySpeakers(ctx, parent, speakerNames)
	meta := channels.VoiceTranscriptSummaryMeta{
		StartedAt:           startedAt,
		EndedAt:             endedAt,
		Duration:            endedAt.Sub(startedAt),
		GuildID:             firstNonEmpty(thread.GuildID, parent.GuildID),
		VoiceChannelID:      c.config.VoiceChannelID,
		VoiceChannelName:    voiceName,
		TranscriptChannelID: thread.ParentID,
		SummaryMessageID:    parent.ID,
		ThreadChannelID:     thread.ID,
		UtteranceCount:      len(lineTimes),
		Speakers:            speakers,
	}

	summary, err := summarizer(ctx, transcript, meta)
	if err != nil {
		return err
	}
	stats := voiceFinalStatsLine(voiceName, meta.Duration, len(speakers), meta.UtteranceCount)
	finalText := stats
	if strings.TrimSpace(summary) != "" {
		discordSummary := voice.FormatSummaryForDiscord(summary, speakers)
		finalText = voice.CombineSummaryAndStats(discordSummary, stats)
	}
	if _, err := c.session.ChannelMessageEdit(thread.ParentID, parent.ID, finalText, discordgo.WithContext(ctx)); err != nil {
		return fmt.Errorf("edit parent summary: %w", err)
	}
	c.editVoiceSummaryRegenerationAck(ctx, thread.ID, ackID, "Voice summary regenerated.")
	return nil
}

func (c *Channel) voiceSummaryParentMessage(ctx context.Context, thread *discordgo.Channel) (*discordgo.Message, error) {
	if thread == nil || thread.ParentID == "" || thread.ID == "" {
		return nil, errors.New("thread parent metadata is missing")
	}
	if msg, err := c.session.ChannelMessage(thread.ParentID, thread.ID, discordgo.WithContext(ctx)); err == nil && msg != nil {
		return msg, nil
	}
	msgs, err := c.session.ChannelMessages(thread.ParentID, 50, "", "", "", discordgo.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("fetch transcript parent messages: %w", err)
	}
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		if msg.ID == thread.ID || (msg.Thread != nil && msg.Thread.ID == thread.ID) {
			return msg, nil
		}
	}
	return nil, errors.New("could not find parent summary message for this transcript thread")
}

func (c *Channel) fetchVoiceTranscriptThreadMessages(ctx context.Context, threadID string) ([]*discordgo.Message, error) {
	var out []*discordgo.Message
	beforeID := ""
	for len(out) < voiceSummaryRegenThreadFetchLimit {
		limit := 100
		if remaining := voiceSummaryRegenThreadFetchLimit - len(out); remaining < limit {
			limit = remaining
		}
		msgs, err := c.session.ChannelMessages(threadID, limit, beforeID, "", "", discordgo.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("fetch transcript thread messages: %w", err)
		}
		if len(msgs) == 0 {
			break
		}
		out = append(out, msgs...)
		beforeID = msgs[len(msgs)-1].ID
		if len(msgs) < limit {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp.Before(out[j].Timestamp)
	})
	return out, nil
}

func voiceTranscriptFromMessages(msgs []*discordgo.Message) (string, []time.Time, []string) {
	var lines []string
	var lineTimes []time.Time
	seenSpeaker := map[string]bool{}
	var speakers []string
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		line := strings.TrimSpace(msg.Content)
		if !looksLikeVoiceTranscriptLine(line) {
			continue
		}
		lines = append(lines, line)
		lineTimes = append(lineTimes, msg.Timestamp)
		if name := voiceTranscriptSpeakerName(line); name != "" && !seenSpeaker[strings.ToLower(name)] {
			seenSpeaker[strings.ToLower(name)] = true
			speakers = append(speakers, name)
		}
	}
	return strings.Join(lines, "\n"), lineTimes, speakers
}

func looksLikeVoiceTranscriptLine(line string) bool {
	idx := strings.Index(line, ":")
	if idx <= 0 || idx > 80 {
		return false
	}
	return strings.TrimSpace(line[idx+1:]) != ""
}

func voiceTranscriptSpeakerName(line string) string {
	idx := strings.Index(line, ":")
	if idx <= 0 || idx > 80 {
		return ""
	}
	return strings.TrimSpace(line[:idx])
}

func voiceSummaryTimeRange(parent *discordgo.Message, lineTimes []time.Time) (time.Time, time.Time) {
	startedAt := time.Time{}
	if parent != nil {
		startedAt = parent.Timestamp
	}
	if len(lineTimes) > 0 && (startedAt.IsZero() || lineTimes[0].Before(startedAt)) {
		startedAt = lineTimes[0]
	}
	endedAt := startedAt
	if len(lineTimes) > 0 {
		endedAt = lineTimes[len(lineTimes)-1]
	}
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	if endedAt.IsZero() || endedAt.Before(startedAt) {
		endedAt = startedAt
	}
	return startedAt, endedAt
}

func (c *Channel) voiceChannelName(ctx context.Context) string {
	if c == nil || c.session == nil || c.config.VoiceChannelID == "" {
		return ""
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ch, err := c.session.Channel(c.config.VoiceChannelID, discordgo.WithContext(lookupCtx))
	if err != nil || ch == nil {
		return ""
	}
	return strings.TrimSpace(ch.Name)
}

func voiceChannelNameFromSummary(content string) string {
	const marker = "Voice session"
	idx := strings.Index(content, marker)
	if idx < 0 {
		return ""
	}
	rest := content[idx+len(marker):]
	for _, prefix := range []string{" ended in ", " started in ", " in "} {
		if i := strings.Index(rest, prefix); i >= 0 {
			value := strings.TrimSpace(rest[i+len(prefix):])
			if cut := strings.Index(value, " — "); cut >= 0 {
				value = value[:cut]
			}
			return strings.TrimPrefix(strings.TrimSpace(value), "#")
		}
	}
	return ""
}

func (c *Channel) voiceSummarySpeakers(ctx context.Context, parent *discordgo.Message, speakerNames []string) []channels.VoiceTranscriptSpeaker {
	mentions := mentionedUserIDs(parent)
	nameToID := map[string]string{}
	guildID := ""
	if parent != nil {
		guildID = parent.GuildID
	}
	for _, userID := range mentions {
		name := c.discordMemberDisplayName(ctx, guildID, userID)
		if name == "" {
			continue
		}
		nameToID[canonicalSpeakerName(name)] = userID
	}
	out := make([]channels.VoiceTranscriptSpeaker, 0, len(speakerNames))
	for _, name := range speakerNames {
		speaker := channels.VoiceTranscriptSpeaker{DisplayName: name}
		if userID := nameToID[canonicalSpeakerName(name)]; userID != "" {
			speaker.UserID = userID
		}
		out = append(out, speaker)
	}
	return out
}

func mentionedUserIDs(msg *discordgo.Message) []string {
	if msg == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, user := range msg.Mentions {
		if user != nil && user.ID != "" && !seen[user.ID] {
			seen[user.ID] = true
			out = append(out, user.ID)
		}
	}
	for _, match := range discordMentionRE.FindAllStringSubmatch(msg.Content, -1) {
		if len(match) < 2 || seen[match[1]] {
			continue
		}
		seen[match[1]] = true
		out = append(out, match[1])
	}
	return out
}

func (c *Channel) discordMemberDisplayName(ctx context.Context, guildID, userID string) string {
	if guildID == "" || userID == "" {
		return ""
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	member, err := c.session.GuildMember(guildID, userID, discordgo.WithContext(lookupCtx))
	if err != nil || member == nil {
		return ""
	}
	if member.Nick != "" {
		return normalizeVoiceSpeakerName(member.Nick)
	}
	if member.User != nil {
		if member.User.GlobalName != "" {
			return normalizeVoiceSpeakerName(member.User.GlobalName)
		}
		if member.User.Username != "" {
			return normalizeVoiceSpeakerName(member.User.Username)
		}
	}
	return ""
}

func normalizeVoiceSpeakerName(name string) string {
	name = strings.TrimSpace(name)
	if before, _, ok := strings.Cut(name, "|"); ok {
		if trimmed := strings.TrimSpace(before); trimmed != "" {
			return trimmed
		}
	}
	return channels.SanitizeDisplayName(name)
}

func canonicalSpeakerName(name string) string {
	return strings.ToLower(strings.TrimSpace(normalizeVoiceSpeakerName(name)))
}

func voiceFinalStatsLine(channelName string, duration time.Duration, speakerCount, utterances int) string {
	label := strings.TrimSpace(channelName)
	if label == "" {
		label = "voice"
	}
	if !strings.HasPrefix(label, "#") {
		label = "#" + label
	}
	return fmt.Sprintf("✅ Voice session ended in %s — %s · %s · %s",
		label,
		formatVoiceDuration(duration),
		pluralizeVoiceStat(speakerCount, "speaker", "speakers"),
		pluralizeVoiceStat(utterances, "utterance", "utterances"),
	)
}

func formatVoiceDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

func pluralizeVoiceStat(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func (c *Channel) editVoiceSummaryRegenerationAck(ctx context.Context, threadID, ackID, content string) {
	if c == nil || c.session == nil || threadID == "" || ackID == "" {
		return
	}
	editCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := c.session.ChannelMessageEdit(threadID, ackID, content, discordgo.WithContext(editCtx)); err != nil {
		slog.Debug("discord: voice summary regeneration ack edit failed", "err", err, "thread_id", threadID)
	}
}
