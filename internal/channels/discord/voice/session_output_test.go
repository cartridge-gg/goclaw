package voice

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cartridge-gg/discordgo"

	"github.com/nextlevelbuilder/goclaw/internal/channels"
)

// On a successful wire-up, the session output posts an initial summary
// message AND creates a thread anchored to it. Subsequent transcript posts
// should go to the thread, not the parent channel.
func Test_newSessionOutput_happy_path_posts_summary_and_creates_thread(t *testing.T) {
	fs := &fakeSession{}
	out := newSessionOutput(context.Background(), fs, "transcript-ch", "voice-ch", "guild-1", discardLogger(), nil)
	if fs.channelCalls != 1 {
		t.Errorf("expected one Channel() lookup for channel name; got %d", fs.channelCalls)
	}
	if fs.channelSendCalls != 1 {
		t.Errorf("expected one ChannelMessageSend for the initial summary; got %d", fs.channelSendCalls)
	}
	if fs.threadStartCalls != 1 {
		t.Errorf("expected one MessageThreadStart; got %d", fs.threadStartCalls)
	}
	if out.summaryMsgID == "" {
		t.Error("summaryMsgID should be populated on successful post")
	}
	if out.threadChannelID == "" {
		t.Error("threadChannelID should be populated on successful thread create")
	}
	if !strings.Contains(fs.lastSentContent, "Voice session started") {
		t.Errorf("summary text should indicate session start: %q", fs.lastSentContent)
	}
	if !strings.Contains(fs.lastSentContent, "#test-channel") {
		t.Errorf("summary should include resolved channel name: %q", fs.lastSentContent)
	}
	if !strings.Contains(fs.lastThreadName, "test-channel") {
		t.Errorf("thread name should include channel name: %q", fs.lastThreadName)
	}
}

// Channel lookup failure → output still usable; summary falls back to the
// bare channel ID for naming but the session can proceed.
func Test_newSessionOutput_channel_lookup_failure_falls_back_to_id(t *testing.T) {
	fs := &fakeSession{
		channelFn: func(_ string) (*discordgo.Channel, error) { return nil, errors.New("no perms") },
	}
	out := newSessionOutput(context.Background(), fs, "transcript-ch", "voice-ch-xyz", "guild-1", discardLogger(), nil)
	if !strings.Contains(fs.lastSentContent, "voice-ch-xyz") {
		t.Errorf("fallback should use raw voice channel ID in summary: %q", fs.lastSentContent)
	}
	// Summary post + thread create still happen.
	if out.summaryMsgID == "" || out.threadChannelID == "" {
		t.Errorf("summary/thread should still be created despite channel-name lookup failure")
	}
}

// Initial summary-post failure → output is in degraded mode. PostLine still
// works by falling through to the parent transcript channel, since there's
// no thread to anchor to.
func Test_newSessionOutput_summary_post_failure_keeps_output_usable(t *testing.T) {
	fs := &fakeSession{
		channelSendFn: func(ch, _ string) (*discordgo.Message, error) {
			if ch == "transcript-ch" {
				return nil, errors.New("forbidden")
			}
			return &discordgo.Message{ID: "msg-x"}, nil
		},
	}
	out := newSessionOutput(context.Background(), fs, "transcript-ch", "voice-ch", "guild-1", discardLogger(), nil)
	if out.summaryMsgID != "" {
		t.Error("summaryMsgID should be empty after failed post")
	}
	if out.threadChannelID != "" {
		t.Error("thread should not have been created after summary post failed")
	}
	// PostLine should still be callable and fall back to parent channel.
	out.PostLine(context.Background(), "alice", "hi there")
	lines := fs.sendsByChannel["transcript-ch"]
	if len(lines) < 1 {
		t.Fatalf("expected at least one parent-channel post after summary failure")
	}
}

func Test_PostLine_recovers_summary_and_thread_after_initial_summary_failure(t *testing.T) {
	summaryAttempts := 0
	fs := &fakeSession{
		channelSendFn: func(ch, content string) (*discordgo.Message, error) {
			if ch == "transcript-ch" && strings.Contains(content, "Voice session started") {
				summaryAttempts++
				if summaryAttempts == 1 {
					return nil, errors.New("context deadline exceeded")
				}
				return &discordgo.Message{ID: "summary-recovered"}, nil
			}
			return &discordgo.Message{ID: "line-msg"}, nil
		},
	}
	out := newSessionOutput(context.Background(), fs, "transcript-ch", "voice-ch", "guild-1", discardLogger(), nil)
	if out.summaryMsgID != "" || out.threadChannelID != "" {
		t.Fatalf("setup should start degraded; got summary=%q thread=%q", out.summaryMsgID, out.threadChannelID)
	}

	out.PostLine(context.Background(), "alice", "hi after recovery")

	if out.summaryMsgID != "summary-recovered" {
		t.Fatalf("expected lazy summary recovery; got %q", out.summaryMsgID)
	}
	if out.threadChannelID != "thread-1" {
		t.Fatalf("expected lazy thread recovery; got %q", out.threadChannelID)
	}
	if got := fs.sendsByChannel["thread-1"]; len(got) != 1 || got[0] != "alice: hi after recovery" {
		t.Fatalf("expected transcript line in recovered thread; got %v", got)
	}
}

// Thread-create failure leaves the output in "parent-channel only" mode.
func Test_newSessionOutput_thread_failure_falls_back_to_parent(t *testing.T) {
	fs := &fakeSession{
		messageThreadStart: func(_, _, _ string, _ int) (*discordgo.Channel, error) {
			return nil, errors.New("rate limit")
		},
	}
	out := newSessionOutput(context.Background(), fs, "transcript-ch", "voice-ch", "guild-1", discardLogger(), nil)
	if out.summaryMsgID == "" {
		t.Error("summary should have been posted before thread create failed")
	}
	if out.threadChannelID != "" {
		t.Error("threadChannelID should stay empty after thread create failed")
	}
	out.PostLine(context.Background(), "alice", "hi")
	// Should post in the PARENT transcript channel (fallback), not a thread.
	if posts := fs.sendsByChannel["transcript-ch"]; len(posts) < 2 {
		t.Fatalf("expected >=2 parent-channel sends (summary + fallback line); got %d", len(posts))
	}
}

func Test_PostLine_recovers_thread_after_initial_thread_failure(t *testing.T) {
	threadAttempts := 0
	fs := &fakeSession{
		messageThreadStart: func(_, _, _ string, _ int) (*discordgo.Channel, error) {
			threadAttempts++
			if threadAttempts == 1 {
				return nil, errors.New("rate limit")
			}
			return &discordgo.Channel{ID: "thread-recovered"}, nil
		},
	}
	out := newSessionOutput(context.Background(), fs, "transcript-ch", "voice-ch", "guild-1", discardLogger(), nil)
	if out.summaryMsgID == "" {
		t.Fatal("summary should have been posted before thread create failed")
	}
	if out.threadChannelID != "" {
		t.Fatalf("setup should start without a thread; got %q", out.threadChannelID)
	}

	out.PostLine(context.Background(), "alice", "hi in thread")

	if out.threadChannelID != "thread-recovered" {
		t.Fatalf("expected lazy thread recovery; got %q", out.threadChannelID)
	}
	if got := fs.sendsByChannel["thread-recovered"]; len(got) != 1 || got[0] != "alice: hi in thread" {
		t.Fatalf("expected transcript line in recovered thread; got %v", got)
	}
	for _, post := range fs.sendsByChannel["transcript-ch"] {
		if post == "alice: hi in thread" {
			t.Fatalf("transcript line should not spill to parent after lazy thread recovery; parent posts: %v", fs.sendsByChannel["transcript-ch"])
		}
	}
}

func Test_newSessionOutput_recovers_active_summary_and_thread(t *testing.T) {
	now := time.Now()
	fs := &fakeSession{
		channelMessagesFn: func(channelID string, limit int, _, _, _ string) ([]*discordgo.Message, error) {
			switch channelID {
			case "transcript-ch":
				return []*discordgo.Message{
					{
						ID:        "summary-old",
						Content:   "🎤 Voice session in #test-channel — 1 speaker: Alice",
						Timestamp: now.Add(-5 * time.Minute),
						Thread:    &discordgo.Channel{ID: "thread-old"},
					},
				}, nil
			case "thread-old":
				return []*discordgo.Message{
					{Content: "Bob: newest line", Timestamp: now.Add(-30 * time.Second)},
					{Content: "Alice: older line", Timestamp: now.Add(-90 * time.Second)},
				}, nil
			default:
				return nil, nil
			}
		},
	}
	out := newSessionOutput(context.Background(), fs, "transcript-ch", "voice-ch", "guild-1", discardLogger(), nil)
	if fs.channelSendCalls != 0 {
		t.Fatalf("recovery should not create a new summary message, sent %d", fs.channelSendCalls)
	}
	if fs.threadStartCalls != 0 {
		t.Fatalf("recovery should not create a new thread, created %d", fs.threadStartCalls)
	}
	if out.summaryMsgID != "summary-old" || out.threadChannelID != "thread-old" {
		t.Fatalf("did not recover existing summary/thread: summary=%q thread=%q", out.summaryMsgID, out.threadChannelID)
	}
	if out.utteranceCount != 2 {
		t.Fatalf("expected recovered utterance count 2, got %d", out.utteranceCount)
	}
	out.PostLine(context.Background(), "Alice", "new line after restart")
	if got := fs.sendsByChannel["thread-old"]; len(got) != 1 {
		t.Fatalf("expected new post to recovered thread, got %d", len(got))
	}
	out.Close(context.Background(), time.Minute)
	if !strings.Contains(fs.lastEditContent, "3 utterances") {
		t.Fatalf("final summary should include recovered + new utterance count: %q", fs.lastEditContent)
	}
}

func Test_newSessionOutput_ignores_stale_active_summary_when_recovering(t *testing.T) {
	stale := time.Now().Add(-12 * time.Hour)
	fs := &fakeSession{
		channelMessagesFn: func(channelID string, _ int, _, _, _ string) ([]*discordgo.Message, error) {
			switch channelID {
			case "transcript-ch":
				return []*discordgo.Message{
					{
						ID:        "summary-old",
						Content:   "🎤 Voice session in #test-channel — 1 speaker: Alice",
						Timestamp: stale,
						Thread:    &discordgo.Channel{ID: "thread-old"},
					},
				}, nil
			case "thread-old":
				return []*discordgo.Message{
					{Content: "Alice: stale line", Timestamp: stale},
				}, nil
			default:
				return nil, nil
			}
		},
	}
	out := newSessionOutput(context.Background(), fs, "transcript-ch", "voice-ch", "guild-1", discardLogger(), nil)
	if out.summaryMsgID == "summary-old" || out.threadChannelID == "thread-old" {
		t.Fatalf("must not recover stale summary/thread: summary=%q thread=%q", out.summaryMsgID, out.threadChannelID)
	}
	if fs.channelSendCalls != 1 || fs.threadStartCalls != 1 {
		t.Fatalf("expected fresh summary/thread after stale recovery candidate, sends=%d threads=%d", fs.channelSendCalls, fs.threadStartCalls)
	}
	if out.utteranceCount != 0 || len(out.transcriptLines) != 0 {
		t.Fatalf("stale transcript lines must not carry into fresh session: count=%d lines=%d", out.utteranceCount, len(out.transcriptLines))
	}
}

func Test_newSessionOutput_recovers_recently_ended_summary_when_thread_is_hot(t *testing.T) {
	now := time.Now()
	var threadMessages []*discordgo.Message
	for i := 0; i < 150; i++ {
		threadMessages = append(threadMessages, &discordgo.Message{
			ID:        fmt.Sprintf("m-%03d", i),
			Content:   fmt.Sprintf("Alice: line %03d", i),
			Timestamp: now.Add(time.Duration(i-149) * time.Second),
		})
	}
	fs := &fakeSession{
		channelMessagesFn: func(channelID string, limit int, beforeID, _, _ string) ([]*discordgo.Message, error) {
			switch channelID {
			case "transcript-ch":
				return []*discordgo.Message{{
					ID:        "summary-ended",
					Content:   "✅ Voice session ended in #test-channel — 36m · 5 speakers · 316 utterances",
					Timestamp: now.Add(-40 * time.Minute),
					Thread:    &discordgo.Channel{ID: "thread-ended"},
				}}, nil
			case "thread-ended":
				// Discord returns messages newest-first. Page one is the newest
				// 100 messages, then beforeID=m-050 returns the older 50.
				var page []*discordgo.Message
				switch beforeID {
				case "":
					for i := len(threadMessages) - 1; i >= 50 && len(page) < limit; i-- {
						page = append(page, threadMessages[i])
					}
				case "m-050":
					for i := 49; i >= 0 && len(page) < limit; i-- {
						page = append(page, threadMessages[i])
					}
				}
				return page, nil
			default:
				return nil, nil
			}
		},
	}
	var seenTranscript string
	summarizer := func(_ context.Context, transcript string, _ channels.VoiceTranscriptSummaryMeta) (string, error) {
		seenTranscript = transcript
		return "Recovered summary.", nil
	}
	out := newSessionOutput(context.Background(), fs, "transcript-ch", "voice-ch", "guild-1", discardLogger(), summarizer)
	if fs.channelSendCalls != 0 || fs.threadStartCalls != 0 {
		t.Fatalf("recovery should not create a new summary/thread, sends=%d threads=%d", fs.channelSendCalls, fs.threadStartCalls)
	}
	if out.summaryMsgID != "summary-ended" || out.threadChannelID != "thread-ended" {
		t.Fatalf("did not recover recently-ended summary/thread: summary=%q thread=%q", out.summaryMsgID, out.threadChannelID)
	}
	if out.utteranceCount != 150 {
		t.Fatalf("expected all paged transcript lines to recover, got %d", out.utteranceCount)
	}
	out.Close(context.Background(), time.Minute)
	first := strings.Index(seenTranscript, "Alice: line 000")
	last := strings.Index(seenTranscript, "Alice: line 149")
	if first < 0 || last < 0 || first > last {
		t.Fatalf("recovered transcript should be chronological and complete, got first=%d last=%d transcript=%q", first, last, seenTranscript)
	}
	if !strings.Contains(fs.lastEditContent, "Recovered summary.") {
		t.Fatalf("final edit should replace stale footer with regenerated summary: %q", fs.lastEditContent)
	}
}

func Test_newSessionOutput_ignores_stale_ended_summary_when_recovering(t *testing.T) {
	stale := time.Now().Add(-12 * time.Hour)
	fs := &fakeSession{
		channelMessagesFn: func(channelID string, _ int, _, _, _ string) ([]*discordgo.Message, error) {
			switch channelID {
			case "transcript-ch":
				return []*discordgo.Message{{
					ID:        "summary-ended",
					Content:   "Discussed shipping.\n\n✅ Voice session ended in #test-channel — 5m · 2 speakers · 8 utterances",
					Timestamp: stale,
					Thread:    &discordgo.Channel{ID: "thread-ended"},
				}}, nil
			case "thread-ended":
				return []*discordgo.Message{{Content: "Alice: stale line", Timestamp: stale}}, nil
			default:
				return nil, nil
			}
		},
	}
	out := newSessionOutput(context.Background(), fs, "transcript-ch", "voice-ch", "guild-1", discardLogger(), nil)
	if out.summaryMsgID == "summary-ended" {
		t.Fatal("must not recover a stale ended voice summary")
	}
	if fs.channelSendCalls != 1 || fs.threadStartCalls != 1 {
		t.Fatalf("expected fresh summary/thread after ignoring ended summary, sends=%d threads=%d", fs.channelSendCalls, fs.threadStartCalls)
	}
}

// NoteSpeaker updates the running summary; repeated calls for the same
// speaker don't re-edit (no change to the list).
func Test_NoteSpeaker_updates_summary_and_dedupes(t *testing.T) {
	fs := &fakeSession{}
	out := newSessionOutput(context.Background(), fs, "transcript-ch", "voice-ch", "guild-1", discardLogger(), nil)
	// Force the edit-throttle timer to the distant past so our edits fire.
	out.lastEditAt = time.Time{}

	out.NoteSpeaker(context.Background(), "u1", "Alice")
	if fs.channelEditCalls != 1 {
		t.Fatalf("first speaker should trigger one summary edit; got %d", fs.channelEditCalls)
	}
	if !strings.Contains(fs.lastEditContent, "Alice") {
		t.Errorf("edit should include Alice: %q", fs.lastEditContent)
	}
	// Same speaker again — no new edit.
	before := fs.channelEditCalls
	out.NoteSpeaker(context.Background(), "u1", "Alice")
	if fs.channelEditCalls != before {
		t.Errorf("repeat same-speaker call should not edit again; got %d -> %d", before, fs.channelEditCalls)
	}
}

// Throttle suppresses mid-burst edits but Close on a non-empty session
// always flushes the final state regardless of recency.
func Test_NoteSpeaker_throttled_but_Close_always_edits(t *testing.T) {
	fs := &fakeSession{}
	out := newSessionOutput(context.Background(), fs, "transcript-ch", "voice-ch", "guild-1", discardLogger(), nil)
	// First speaker: open the throttle window.
	out.NoteSpeaker(context.Background(), "u1", "Alice")
	// Second speaker immediately — within the throttle window.
	out.NoteSpeaker(context.Background(), "u2", "Bob")
	// Between the two NoteSpeaker calls we expect at MOST one edit (the
	// first), because the second was suppressed by the throttle.
	if fs.channelEditCalls > 1 {
		t.Errorf("expected throttle to suppress second edit; got %d edits", fs.channelEditCalls)
	}
	// Post a transcript line so Close takes the non-empty path. With
	// utteranceCount==0 Close deletes the summary instead of editing
	// (verified separately in Test_Close_empty_session_deletes_summary).
	out.PostLine(context.Background(), "Alice", "hi")
	editsBefore := fs.channelEditCalls
	out.Close(context.Background(), 42*time.Second)
	if fs.channelEditCalls <= editsBefore {
		t.Fatalf("Close on non-empty session should fire a final edit; calls %d -> %d", editsBefore, fs.channelEditCalls)
	}
	if !strings.Contains(fs.lastEditContent, "ended") {
		t.Errorf("Close should edit with 'ended' marker: %q", fs.lastEditContent)
	}
	if !strings.Contains(fs.lastEditContent, "2 speakers") {
		t.Errorf("Close should report 2 speakers: %q", fs.lastEditContent)
	}
}

// Close is idempotent and safe on a nil or uninitialized output.
// Empty session (no PostLine calls) deletes the summary + thread on
// the first Close; second Close is a no-op.
func Test_Close_idempotent_and_nil_safe(t *testing.T) {
	var nilOut *sessionOutput
	nilOut.Close(context.Background(), 0) // nil receiver is allowed

	fs := &fakeSession{}
	out := newSessionOutput(context.Background(), fs, "transcript-ch", "voice-ch", "guild-1", discardLogger(), nil)
	beforeDel := fs.channelMessageDeleteCalls
	beforeChDel := fs.channelDeleteCalls
	out.Close(context.Background(), time.Second)
	afterDel1 := fs.channelMessageDeleteCalls
	afterChDel1 := fs.channelDeleteCalls
	out.Close(context.Background(), time.Second)
	afterDel2 := fs.channelMessageDeleteCalls
	afterChDel2 := fs.channelDeleteCalls
	if afterDel1 <= beforeDel {
		t.Errorf("first Close on empty session should delete the summary message; got %d -> %d", beforeDel, afterDel1)
	}
	if afterChDel1 <= beforeChDel {
		t.Errorf("first Close on empty session should delete the thread; got %d -> %d", beforeChDel, afterChDel1)
	}
	if afterDel2 != afterDel1 || afterChDel2 != afterChDel1 {
		t.Errorf("second Close should be a no-op; deletes %d -> %d, channel deletes %d -> %d",
			afterDel1, afterDel2, afterChDel1, afterChDel2)
	}
}

// Empty session (zero utterances): Close deletes both the parent
// summary message and the attached thread to keep the transcript
// channel quiet for sessions where no human spoke.
func Test_Close_empty_session_deletes_summary_and_thread(t *testing.T) {
	fs := &fakeSession{}
	out := newSessionOutput(context.Background(), fs, "transcript-ch", "voice-ch", "guild-1", discardLogger(), nil)
	editsBefore := fs.channelEditCalls
	out.Close(context.Background(), time.Minute)
	if fs.channelMessageDeleteCalls != 1 {
		t.Errorf("empty session should delete exactly one summary message; got %d", fs.channelMessageDeleteCalls)
	}
	if fs.channelDeleteCalls != 1 {
		t.Errorf("empty session should delete exactly one thread; got %d", fs.channelDeleteCalls)
	}
	if fs.channelEditCalls != editsBefore {
		t.Errorf("empty session should NOT edit the summary (just delete); edits %d -> %d", editsBefore, fs.channelEditCalls)
	}
	if fs.lastDeletedChannelID != "transcript-ch" {
		t.Errorf("summary delete should target the transcript channel; got %q", fs.lastDeletedChannelID)
	}
}

// When a TranscriptSummarizer is configured and the session has at
// least one transcribed utterance, Close runs the summarizer and uses
// its output as the final summary message body (with the stats line
// appended).
func Test_Close_runs_summarizer_when_set(t *testing.T) {
	fs := &fakeSession{}
	called := false
	var seenTranscript string
	summarizer := func(_ context.Context, transcript string, _ channels.VoiceTranscriptSummaryMeta) (string, error) {
		called = true
		seenTranscript = transcript
		return "Discussed the new feature rollout.", nil
	}
	out := newSessionOutput(context.Background(), fs, "transcript-ch", "voice-ch", "guild-1", discardLogger(), summarizer)
	out.PostLine(context.Background(), "Alice", "we're rolling out the new feature next week")
	out.PostLine(context.Background(), "Bob", "anything I should help with?")
	out.Close(context.Background(), 5*time.Minute)
	if !called {
		t.Fatal("summarizer should have been invoked")
	}
	if !strings.Contains(seenTranscript, "Alice: we're rolling out") {
		t.Errorf("summarizer received unexpected transcript: %q", seenTranscript)
	}
	if !strings.Contains(seenTranscript, "Bob: anything I should help with") {
		t.Errorf("summarizer should see all lines: %q", seenTranscript)
	}
	if !strings.Contains(fs.lastEditContent, "Discussed the new feature rollout.") {
		t.Errorf("summary edit should contain summarizer output: %q", fs.lastEditContent)
	}
	if !strings.Contains(fs.lastEditContent, "ended") {
		t.Errorf("summary edit should still include stats line: %q", fs.lastEditContent)
	}
}

func Test_Close_formats_summary_for_discord(t *testing.T) {
	fs := &fakeSession{}
	var seenMeta channels.VoiceTranscriptSummaryMeta
	summarizer := func(_ context.Context, _ string, meta channels.VoiceTranscriptSummaryMeta) (string, error) {
		seenMeta = meta
		return "alice and [[bob]] discussed [[Controller|Controller]].", nil
	}
	out := newSessionOutput(context.Background(), fs, "transcript-ch", "voice-ch", "guild-1", discardLogger(), summarizer)
	out.NoteSpeaker(context.Background(), "111111", "alice")
	out.NoteSpeaker(context.Background(), "222222", "bob")
	out.PostLine(context.Background(), "alice", "we should fix this")
	out.Close(context.Background(), 2*time.Minute)

	if strings.Contains(fs.lastEditContent, "[[") || strings.Contains(fs.lastEditContent, "]]") {
		t.Fatalf("Discord summary should not contain wikilink brackets: %q", fs.lastEditContent)
	}
	for _, want := range []string{"<@111111>", "<@222222>", "Controller"} {
		if !strings.Contains(fs.lastEditContent, want) {
			t.Fatalf("Discord summary missing %q: %q", want, fs.lastEditContent)
		}
	}
	if seenMeta.GuildID != "guild-1" || seenMeta.SummaryMessageID == "" || len(seenMeta.Speakers) != 2 {
		t.Fatalf("summarizer metadata not populated: %+v", seenMeta)
	}
}

func Test_RenderFinalSummaryForDiscord_splits_action_items_into_embeds(t *testing.T) {
	stats := "✅ Voice session ended in #chill — 35m · 2 speakers · 120 utterances"
	summary := strings.Join([]string{
		"[[Controller]] launch scope tightened around the account flow.",
		"",
		"Action items:",
		"- alice: mock the iOS onboarding variant.",
		"- Unassigned: decide whether PvP stays in MVP.",
	}, "\n")
	got := RenderFinalSummaryForDiscord(summary, stats, []channels.VoiceTranscriptSpeaker{
		{UserID: "111111", DisplayName: "alice"},
	})

	if got.Content != stats {
		t.Fatalf("content = %q, want stats line", got.Content)
	}
	if len(got.Embeds) != 2 {
		t.Fatalf("embeds len = %d, want summary + tasks", len(got.Embeds))
	}
	if got.Embeds[0].Title != "Session summary" || !strings.Contains(got.Embeds[0].Description, "Controller launch scope") {
		t.Fatalf("summary embed not populated: %+v", got.Embeds[0])
	}
	if strings.Contains(got.Embeds[0].Description, "Action items") {
		t.Fatalf("summary embed should not duplicate action section: %q", got.Embeds[0].Description)
	}
	if got.Embeds[1].Title != "Proposed tasks" || len(got.Embeds[1].Fields) != 2 {
		t.Fatalf("task embed not populated: %+v", got.Embeds[1])
	}
	if got.Embeds[1].Fields[0].Name != "<@111111>" {
		t.Fatalf("owner should be converted to mention, got %q", got.Embeds[1].Fields[0].Name)
	}
	if !strings.Contains(got.FallbackContent, "Action items") {
		t.Fatalf("plain fallback should retain action items: %q", got.FallbackContent)
	}
}

// Summarizer returning an error → fall back to the legacy stats line.
func Test_Close_summarizer_error_falls_back_to_stats(t *testing.T) {
	fs := &fakeSession{}
	summarizer := func(_ context.Context, _ string, _ channels.VoiceTranscriptSummaryMeta) (string, error) {
		return "", errors.New("provider down")
	}
	out := newSessionOutput(context.Background(), fs, "transcript-ch", "voice-ch", "guild-1", discardLogger(), summarizer)
	out.PostLine(context.Background(), "Alice", "hi")
	out.Close(context.Background(), time.Minute)
	if strings.Contains(fs.lastEditContent, "provider") {
		t.Errorf("error string should not leak into summary: %q", fs.lastEditContent)
	}
	if !strings.Contains(fs.lastEditContent, "1 utterance") {
		t.Errorf("fallback should include stats line: %q", fs.lastEditContent)
	}
}

func Test_Close_summarizer_timeout_still_posts_final_stats(t *testing.T) {
	oldSummaryTimeout := transcriptSummaryTimeout
	oldEditTimeout := finalSummaryEditTimeout
	transcriptSummaryTimeout = 10 * time.Millisecond
	finalSummaryEditTimeout = 50 * time.Millisecond
	defer func() {
		transcriptSummaryTimeout = oldSummaryTimeout
		finalSummaryEditTimeout = oldEditTimeout
	}()

	fs := &fakeSession{}
	summarizer := func(ctx context.Context, _ string, _ channels.VoiceTranscriptSummaryMeta) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	out := newSessionOutput(context.Background(), fs, "transcript-ch", "voice-ch", "guild-1", discardLogger(), summarizer)
	out.PostLine(context.Background(), "Alice", "hi")

	closeCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	out.Close(closeCtx, time.Minute)

	if fs.channelEditCalls == 0 {
		t.Fatal("final stats edit should still be attempted after summarizer timeout")
	}
	if !strings.Contains(fs.lastEditContent, "✅ Voice session ended") {
		t.Fatalf("fallback stats should be posted after summarizer timeout: %q", fs.lastEditContent)
	}
}

func Test_Close_truncates_overlong_summarizer_output(t *testing.T) {
	fs := &fakeSession{}
	summarizer := func(_ context.Context, _ string, _ channels.VoiceTranscriptSummaryMeta) (string, error) {
		return strings.Repeat("x", summaryMessageMaxLen+500), nil
	}
	out := newSessionOutput(context.Background(), fs, "transcript-ch", "voice-ch", "guild-1", discardLogger(), summarizer)
	out.PostLine(context.Background(), "Alice", "hi")
	out.Close(context.Background(), time.Minute)
	if len(fs.lastEditContent) > summaryMessageMaxLen {
		t.Fatalf("summary edit content length = %d, want <= %d", len(fs.lastEditContent), summaryMessageMaxLen)
	}
	if !strings.Contains(fs.lastEditContent, "✅ Voice session ended") {
		t.Fatalf("truncated summary should preserve stats line: %q", fs.lastEditContent)
	}
}

// With no summaryMsgID (initial post failed), Close is a clean no-op.
func Test_Close_noop_when_summary_post_failed(t *testing.T) {
	fs := &fakeSession{
		channelSendFn: func(_, _ string) (*discordgo.Message, error) {
			return nil, errors.New("forbidden")
		},
	}
	out := newSessionOutput(context.Background(), fs, "transcript-ch", "voice-ch", "guild-1", discardLogger(), nil)
	before := fs.channelEditCalls
	out.Close(context.Background(), time.Minute)
	if fs.channelEditCalls != before {
		t.Errorf("Close should not edit when summary post never succeeded; got %d -> %d", before, fs.channelEditCalls)
	}
}

// PostLine records utterances in utteranceCount, exposed through the final
// summary's stats line.
func Test_Close_reports_utterance_count(t *testing.T) {
	fs := &fakeSession{}
	out := newSessionOutput(context.Background(), fs, "transcript-ch", "voice-ch", "guild-1", discardLogger(), nil)
	for i := 0; i < 3; i++ {
		out.PostLine(context.Background(), "alice", "hi")
	}
	out.Close(context.Background(), 90*time.Second)
	if !strings.Contains(fs.lastEditContent, "3 utterances") {
		t.Errorf("final summary should report 3 utterances: %q", fs.lastEditContent)
	}
	if !strings.Contains(fs.lastEditContent, "1m") {
		t.Errorf("final summary should include duration: %q", fs.lastEditContent)
	}
}

func Test_PostLine_retains_transcript_when_discord_post_fails(t *testing.T) {
	fs := &fakeSession{
		channelSendFn: func(channelID, _ string) (*discordgo.Message, error) {
			if channelID == "thread-1" {
				return nil, errors.New("discord timeout")
			}
			return &discordgo.Message{ID: "summary-1"}, nil
		},
	}
	var seenTranscript string
	summarizer := func(_ context.Context, transcript string, _ channels.VoiceTranscriptSummaryMeta) (string, error) {
		seenTranscript = transcript
		return "summary", nil
	}
	out := newSessionOutput(context.Background(), fs, "transcript-ch", "voice-ch", "guild-1", discardLogger(), summarizer)
	out.PostLine(context.Background(), "Alice", "this should survive")
	out.Close(context.Background(), time.Minute)

	if !strings.Contains(seenTranscript, "Alice: this should survive") {
		t.Fatalf("failed Discord posts should still feed the final summary, got %q", seenTranscript)
	}
	if !strings.Contains(fs.lastEditContent, "1 utterance") {
		t.Fatalf("final stats should count accepted transcript lines: %q", fs.lastEditContent)
	}
}

// The thread-message cap spills overflow to the parent transcript channel
// so late-session transcripts still reach operators.
func Test_PostLine_falls_back_to_parent_on_thread_cap(t *testing.T) {
	fs := &fakeSession{}
	out := newSessionOutput(context.Background(), fs, "transcript-ch", "voice-ch", "guild-1", discardLogger(), nil)
	out.mu.Lock()
	out.utteranceCount = threadMessageCap // simulate cap already hit
	out.mu.Unlock()

	out.PostLine(context.Background(), "alice", "hi after cap")
	// At least one post must have landed in the parent transcript channel
	// beyond the initial summary.
	parentPosts := fs.sendsByChannel["transcript-ch"]
	foundFallback := false
	for _, p := range parentPosts {
		if strings.Contains(p, "hi after cap") {
			foundFallback = true
			break
		}
	}
	if !foundFallback {
		t.Errorf("post-cap line should have spilled to parent channel; parent posts: %v", parentPosts)
	}
}

// Regression guard: nil receivers on the hot-path calls don't panic.
func Test_sessionOutput_nil_methods_are_safe(t *testing.T) {
	var out *sessionOutput
	out.PostLine(context.Background(), "a", "b")
	out.NoteSpeaker(context.Background(), "u", "n")
	out.Close(context.Background(), 0)
	// If any of the above panicked, the test would have already failed.
}
