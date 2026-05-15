package channels

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/nextlevelbuilder/goclaw/internal/providers"
)

// defaultVoiceSummaryPrompt is the fallback system prompt when no
// skill body is configured on the summarizer. It asks for a summary
// whose depth scales with the session length while staying Discord-
// friendly. Project-agnostic — references no specific people or
// products.
const defaultVoiceSummaryPrompt = `You are summarizing a Discord voice channel conversation for a transcript channel.

The user will provide session metadata and a transcript with each line in the form "<speaker name>: <what they said>". Your job is to produce a useful summary suitable for a Discord channel message and an Obsidian memory note.

Guidelines:
- Scale detail to the call: short/content-light calls can be 2-4 bullets or paragraphs; 10-30 minute calls should usually be 4-8 bullets or paragraphs; 30+ minute calls should usually be 8-14 bullets or paragraphs with meaningful decisions, tradeoffs, and unresolved questions.
- Lead with the topic, decision, or outcome, not generic preamble.
- Mention speakers by name when attribution matters; omit names for filler.
- Quote at most one short, distinctive line if it's load-bearing.
- Capture concrete tasks, owners, assignments, follow-ups, and proposed next steps.
- If tasks were discussed, end with a section headed exactly "Action items:" and format each task as "- Owner: task". Use the exact transcript speaker name as Owner when assigned; use "Unassigned" when no owner is clear. Keep uncertainty in the task text instead of inventing ownership.
- Skip filler ("uh", "you know"), greetings, and side-channel chatter.
- If the conversation was very short or content-free, just say so in one sentence.
- Use plain text — no Markdown headings; small inline emphasis is fine.

Stay under 3500 characters total.`

// voiceSummaryNoToolGuard is appended even when a custom skill body replaces
// the default prompt. Voice summaries call the provider directly, outside the
// agent tool loop, so any tool-call markup in the model output would be posted
// verbatim to Discord.
const voiceSummaryNoToolGuard = `You do not have tools in this task. Do not call memory_search or any other tool. Do not emit XML, DSML, JSON tool calls, or tool-call markup. Return only the human-readable summary text.`

// memoryContextHeader prefaces injected memory snippets in the system
// prompt so the model knows it's looking at supplemental context, not
// the transcript itself.
const memoryContextHeader = `\n\n--- Memory context (use these to ground names + topics; do NOT quote verbatim) ---`

// BuildVoiceTranscriptSummarizer wraps a VoiceTranscriptSummarizerConfig
// into a closure suitable for voice.Config.TranscriptSummarizer. The
// returned function:
//
//  1. (Optional) Queries the agent's memory for context relevant to the
//     transcript — surfaces contributor pages, project pages, recent
//     prior voice-session entries — and injects the snippets into the
//     system prompt so the model can normalize names + cite prior
//     activity.
//  2. Calls the configured provider's Chat endpoint with the system
//     prompt (skill body if configured, else default) + transcript.
//  3. (Optional) Writes the resulting summary to a memory file at
//     <session_output_dir>/<YYYY-MM-DD>/<HHMM>-<channel>.md so future
//     sessions inherit it as temporal context.
//
// Returns nil if cfg is nil or has no Provider — callers treat nil as
// "no summarizer wired" and fall back to the legacy stats line.
func BuildVoiceTranscriptSummarizer(cfg *VoiceTranscriptSummarizerConfig) func(ctx context.Context, transcript string, meta VoiceTranscriptSummaryMeta) (string, error) {
	if cfg == nil || cfg.Provider == nil || cfg.Model == "" {
		return nil
	}
	maxTokens := cfg.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	thinkingLevel := cfg.ThinkingLevel
	if thinkingLevel == "" {
		thinkingLevel = "low"
	}
	provider := cfg.Provider
	model := cfg.Model

	systemPrompt := strings.TrimSpace(cfg.SkillBody)
	if systemPrompt == "" {
		systemPrompt = defaultVoiceSummaryPrompt
	}

	return func(ctx context.Context, transcript string, meta VoiceTranscriptSummaryMeta) (string, error) {
		t := strings.TrimSpace(transcript)
		if t == "" {
			return "", errors.New("voice summarizer: empty transcript")
		}

		augmented := systemPrompt
		augmented = augmented + "\n\n" + voiceSummaryNoToolGuard
		if metaBlock := voiceSummaryMetadataBlock(meta); metaBlock != "" {
			augmented = augmented + "\n\n" + metaBlock
		}
		if cfg.MemoryStore != nil && cfg.MemoryAgentID != "" {
			if ctxBlob := buildMemoryContext(ctx, cfg, t); ctxBlob != "" {
				augmented = augmented + memoryContextHeader + "\n" + ctxBlob
			}
		}

		// Notes on options: see PR #41 — gpt-5 family reject explicit
		// temperature; reasoning models burn max_completion_tokens on
		// reasoning before output (low budget needs explicit thinking
		// level control); non-reasoning models silently ignore the
		// thinking_level option.
		resp, err := provider.Chat(ctx, providers.ChatRequest{
			Messages: []providers.Message{
				{Role: "system", Content: augmented},
				{Role: "user", Content: t},
			},
			Model: model,
			Options: map[string]any{
				"max_tokens":     maxTokens,
				"thinking_level": thinkingLevel,
			},
		})
		if err != nil {
			return "", fmt.Errorf("voice summarizer chat: %w", err)
		}
		if resp == nil {
			return "", errors.New("voice summarizer chat: nil response")
		}
		if responseHasToolCalls(resp) {
			slog.Warn("voice summarizer: provider returned tool calls; falling back to stats line",
				"finish_reason", resp.FinishReason, "tool_calls", len(resp.ToolCalls))
			return "", nil
		}

		summary := strings.TrimSpace(resp.Content)
		if summary == "" {
			return "", nil // caller logs + falls back to stats line
		}
		if containsToolCallMarkup(summary) {
			slog.Warn("voice summarizer: provider returned tool-call markup; falling back to stats line")
			return "", nil
		}

		// Best-effort: write the new session summary to memory so the
		// next session benefits. Failures here log + continue — the
		// summary still gets posted to Discord even if memory write
		// fails. The disk seeder picks up the new file on its next
		// sweep (see internal/memory/disk_seeder.go).
		if cfg.SessionOutputDir != "" && cfg.MemoryStore != nil && cfg.MemoryAgentID != "" && cfg.MemoryWorkspace != "" {
			if err := persistSessionSummary(ctx, cfg, summary, meta); err != nil {
				slog.Warn("voice summarizer: persist memory failed", "err", err)
			}
		}

		return summary, nil
	}
}

func voiceSummaryMetadataBlock(meta VoiceTranscriptSummaryMeta) string {
	var b strings.Builder
	b.WriteString("--- Session metadata ---\n")
	wrote := false
	if meta.Duration > 0 {
		fmt.Fprintf(&b, "duration: %s\n", meta.Duration.Round(time.Second))
		wrote = true
	}
	if meta.UtteranceCount > 0 {
		fmt.Fprintf(&b, "utterances: %d\n", meta.UtteranceCount)
		wrote = true
	}
	if len(meta.Speakers) > 0 {
		names := make([]string, 0, len(meta.Speakers))
		for _, speaker := range meta.Speakers {
			name := strings.TrimSpace(speaker.DisplayName)
			if name != "" {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			fmt.Fprintf(&b, "speakers: %s\n", strings.Join(names, ", "))
			wrote = true
		}
	}
	if !wrote {
		return ""
	}
	return strings.TrimSpace(b.String())
}

func responseHasToolCalls(resp *providers.ChatResponse) bool {
	if resp == nil {
		return false
	}
	return resp.FinishReason == "tool_calls" || len(resp.ToolCalls) > 0
}

func containsToolCallMarkup(s string) bool {
	if s == "" {
		return false
	}
	lower := strings.ToLower(s)
	markers := []string{
		"<｜dsml｜tool_calls",
		"<｜dsml｜invoke",
		"</｜dsml｜tool_calls",
		"<tool_calls",
		"</tool_calls",
		"<tool_call",
		"</tool_call",
		"\"tool_calls\"",
	}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// buildMemoryContext queries the agent's memory for snippets relevant
// to the transcript. Returns an empty string when no useful context
// surfaces. Strategy: take the transcript's first 4 KiB (most calls
// open with the topic), search semantically, then also search likely
// project/product/entity names and each unique speaker prefix. The LLM
// still gets no live tools; memory lookup happens here so summaries can
// use org context without leaking tool-call markup to Discord.
func buildMemoryContext(ctx context.Context, cfg *VoiceTranscriptSummarizerConfig, transcript string) string {
	const transcriptHead = 4096
	query := transcript
	if len(query) > transcriptHead {
		query = query[:transcriptHead]
	}

	// Topical search.
	topical, err := cfg.MemoryStore.Search(ctx, query, cfg.MemoryAgentID, "", MemorySearchOpts{MaxResults: 5})
	if err != nil {
		slog.Debug("voice summarizer: topical memory search failed", "err", err)
	}

	// Per-speaker search (canonical name lookup).
	speakers := uniqueSpeakers(transcript)
	var byPath = map[string]MemorySnippet{}
	for _, s := range topical {
		byPath[s.Path] = s
	}
	for _, q := range contextLookupQueries(transcript) {
		got, err := cfg.MemoryStore.Search(ctx, q, cfg.MemoryAgentID, "", MemorySearchOpts{MaxResults: 2})
		if err != nil {
			continue
		}
		for _, s := range got {
			if _, dup := byPath[s.Path]; !dup {
				byPath[s.Path] = s
			}
		}
	}
	for _, name := range speakers {
		got, err := cfg.MemoryStore.Search(ctx, name, cfg.MemoryAgentID, "", MemorySearchOpts{MaxResults: 2})
		if err != nil {
			continue
		}
		for _, s := range got {
			if _, dup := byPath[s.Path]; !dup {
				byPath[s.Path] = s
			}
		}
	}
	if len(byPath) == 0 {
		return ""
	}
	// Stable ordering (by score desc then path).
	all := make([]MemorySnippet, 0, len(byPath))
	for _, s := range byPath {
		all = append(all, s)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Score != all[j].Score {
			return all[i].Score > all[j].Score
		}
		return all[i].Path < all[j].Path
	})

	// Cap final injection size — memory snippets are useful but
	// blowing the prompt budget defeats the point.
	const maxBytes = 6000
	var b strings.Builder
	for _, s := range all {
		section := fmt.Sprintf("[%s] %s\n", s.Path, strings.TrimSpace(s.Snippet))
		if b.Len()+len(section) > maxBytes {
			break
		}
		b.WriteString(section)
	}
	return b.String()
}

func contextLookupQueries(transcript string) []string {
	const maxScanBytes = 8192
	const maxQueries = 10
	if len(transcript) > maxScanBytes {
		transcript = transcript[:maxScanBytes]
	}

	var out []string
	seen := map[string]bool{}
	add := func(q string) {
		if len(out) >= maxQueries {
			return
		}
		q = strings.TrimSpace(q)
		q = strings.Trim(q, `"'`+"`.,;:!?()[]{}<>")
		if q == "" || len(q) > 80 {
			return
		}
		key := strings.ToLower(q)
		if seen[key] || isContextStopWord(key) {
			return
		}
		seen[key] = true
		out = append(out, q+" product project organization context")
	}

	for _, line := range strings.Split(transcript, "\n") {
		text := stripSpeakerPrefix(line)
		words := contextWords(text)
		for i, w := range words {
			if isProjectLikeToken(w) {
				add(w)
			}
			if isContextCue(w) && i+1 < len(words) {
				add(nextContextPhrase(words[i+1:]))
			}
		}
		for _, phrase := range capitalizedContextPhrases(words) {
			add(phrase)
		}
		if len(out) >= maxQueries {
			break
		}
	}
	return out
}

func stripSpeakerPrefix(line string) string {
	idx := strings.Index(line, ":")
	if idx <= 0 || idx > 80 {
		return line
	}
	return line[idx+1:]
}

func contextWords(s string) []string {
	raw := strings.Fields(s)
	out := make([]string, 0, len(raw))
	for _, w := range raw {
		w = strings.Trim(w, `"'`+"`.,;:!?()[]{}<>")
		if w == "" || !hasLetter(w) {
			continue
		}
		out = append(out, w)
	}
	return out
}

func isProjectLikeToken(w string) bool {
	if len(w) < 3 || isContextStopWord(strings.ToLower(w)) {
		return false
	}
	if strings.ContainsAny(w, "-_./") {
		return true
	}
	return startsUpper(w) || hasInnerUpper(w) || isAcronym(w)
}

func isContextCue(w string) bool {
	switch strings.ToLower(w) {
	case "app", "api", "chain", "client", "contract", "feature", "package", "platform", "product", "project", "protocol", "repo", "repository", "sdk", "service", "system":
		return true
	default:
		return false
	}
}

func nextContextPhrase(words []string) string {
	parts := make([]string, 0, 3)
	for _, w := range words {
		lower := strings.ToLower(w)
		if isContextStopWord(lower) || isContextCue(w) {
			if len(parts) == 0 {
				continue
			}
			break
		}
		parts = append(parts, w)
		if len(parts) == 3 {
			break
		}
	}
	return strings.Join(parts, " ")
}

func capitalizedContextPhrases(words []string) []string {
	var out []string
	for i := 0; i < len(words); i++ {
		if !startsUpper(words[i]) || isContextStopWord(strings.ToLower(words[i])) {
			continue
		}
		parts := []string{words[i]}
		for j := i + 1; j < len(words) && len(parts) < 4; j++ {
			if !startsUpper(words[j]) || isContextStopWord(strings.ToLower(words[j])) {
				break
			}
			parts = append(parts, words[j])
		}
		out = append(out, strings.Join(parts, " "))
		i += len(parts) - 1
	}
	return out
}

func hasLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

func startsUpper(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return unicode.IsUpper(r)
		}
	}
	return false
}

func hasInnerUpper(s string) bool {
	seenLetter := false
	for _, r := range s {
		if !unicode.IsLetter(r) {
			continue
		}
		if seenLetter && unicode.IsUpper(r) {
			return true
		}
		seenLetter = true
	}
	return false
}

func isAcronym(s string) bool {
	letters := 0
	for _, r := range s {
		if !unicode.IsLetter(r) {
			continue
		}
		letters++
		if !unicode.IsUpper(r) {
			return false
		}
	}
	return letters >= 2 && letters <= 8
}

func isContextStopWord(s string) bool {
	switch s {
	case "a", "about", "an", "and", "are", "as", "at", "be", "but", "by", "for", "from", "i", "if", "in", "is", "it", "like", "need", "needed", "needs", "of", "on", "or", "our", "so", "that", "the", "their", "then", "this", "to", "we", "with", "you":
		return true
	default:
		return false
	}
}

// uniqueSpeakers extracts the unique "<DisplayName>:" prefixes from
// the transcript so we can do per-speaker memory lookups for
// canonical-name resolution.
func uniqueSpeakers(transcript string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(transcript, "\n") {
		idx := strings.Index(line, ":")
		if idx <= 0 || idx > 80 {
			continue
		}
		name := strings.TrimSpace(line[:idx])
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// persistSessionSummary writes the new summary to a memory file under
// <workspace>/memory/<session_output_dir>/<YYYY-MM-DD>/<HHMM>-<channel>.md
// with Obsidian-style frontmatter. The disk seeder's next sweep
// indexes it; future sessions can find it via memory_search.
func persistSessionSummary(ctx context.Context, cfg *VoiceTranscriptSummarizerConfig, summary string, meta VoiceTranscriptSummaryMeta) error {
	now := meta.EndedAt
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	dateDir := now.Format("2006-01-02")
	channelSlug := slugifyVoiceSummaryPath(meta.VoiceChannelName)
	if channelSlug == "" {
		channelSlug = slugifyVoiceSummaryPath(meta.VoiceChannelID)
	}
	if channelSlug == "" {
		channelSlug = "session"
	}
	fileName := now.Format("1504") + "-" + channelSlug + ".md"
	relPath := filepath.ToSlash(filepath.Join("memory", cfg.SessionOutputDir, dateDir, fileName))

	body := buildVoiceSessionMemoryNote(summary, meta, now)

	// Write through the MemoryStore so it lands in memory_documents +
	// gets indexed; the next disk sweep is what brings it onto disk
	// via the agent's filesystem only IF the consumer has the disk
	// seeder configured the other direction. For now we just persist
	// via the store — the file-on-disk parity step is downstream.
	if err := cfg.MemoryStore.PutDocument(ctx, cfg.MemoryAgentID, "", relPath, body); err != nil {
		return fmt.Errorf("put summary doc: %w", err)
	}

	// Also write to disk so the seeder's hash-shortcircuit doesn't
	// re-index it on every sweep — the file IS the source of truth.
	if cfg.MemoryWorkspace != "" {
		absPath := filepath.Join(cfg.MemoryWorkspace, relPath)
		if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
			return fmt.Errorf("mkdir for summary: %w", err)
		}
		if err := os.WriteFile(absPath, []byte(body), 0o644); err != nil {
			return fmt.Errorf("write summary file: %w", err)
		}
	}
	return nil
}

func buildVoiceSessionMemoryNote(summary string, meta VoiceTranscriptSummaryMeta, endedAt time.Time) string {
	if endedAt.IsZero() {
		endedAt = time.Now().UTC()
	}
	startedAt := meta.StartedAt
	if startedAt.IsZero() {
		startedAt = endedAt
	}
	startedAt = startedAt.UTC()
	endedAt = endedAt.UTC()

	titleChannel := meta.VoiceChannelName
	if titleChannel == "" {
		titleChannel = meta.VoiceChannelID
	}
	if titleChannel == "" {
		titleChannel = "Voice"
	}
	title := fmt.Sprintf("%s voice session - %s", titleChannel, endedAt.Format("2006-01-02 15:04 UTC"))

	var b strings.Builder
	fmt.Fprintf(&b, "---\n")
	fmt.Fprintf(&b, "title: %s\n", yamlQuote(title))
	fmt.Fprintf(&b, "type: voice-session\n")
	fmt.Fprintf(&b, "created: %s\n", yamlQuote(startedAt.Format(time.RFC3339)))
	fmt.Fprintf(&b, "updated: %s\n", yamlQuote(endedAt.Format(time.RFC3339)))
	fmt.Fprintf(&b, "date: %s\n", yamlQuote(endedAt.Format("2006-01-02")))
	fmt.Fprintf(&b, "source: discord\n")
	fmt.Fprintf(&b, "channel: %s\n", yamlQuote(titleChannel))
	if meta.VoiceChannelID != "" {
		fmt.Fprintf(&b, "channel_id: %s\n", yamlQuote(meta.VoiceChannelID))
	}
	if meta.GuildID != "" {
		fmt.Fprintf(&b, "guild_id: %s\n", yamlQuote(meta.GuildID))
	}
	if meta.ThreadChannelID != "" {
		fmt.Fprintf(&b, "thread_id: %s\n", yamlQuote(meta.ThreadChannelID))
	}
	fmt.Fprintf(&b, "duration_seconds: %d\n", int(meta.Duration.Seconds()))
	fmt.Fprintf(&b, "utterances: %d\n", meta.UtteranceCount)
	fmt.Fprintf(&b, "participants:\n")
	for _, speaker := range meta.Speakers {
		name := strings.TrimSpace(speaker.DisplayName)
		if name == "" {
			continue
		}
		fmt.Fprintf(&b, "  - %s\n", yamlQuote("[["+name+"]]"))
	}
	if source := discordSummaryURL(meta); source != "" {
		fmt.Fprintf(&b, "sources:\n  - %s\n", yamlQuote(source))
	}
	if links := summaryWikilinks(summary); len(links) > 0 {
		fmt.Fprintf(&b, "related:\n")
		for _, link := range links {
			fmt.Fprintf(&b, "  - %s\n", yamlQuote("[["+link+"]]"))
		}
	}
	fmt.Fprintf(&b, "tags:\n  - voice\n  - discord\n")
	fmt.Fprintf(&b, "---\n\n")
	if strings.TrimSpace(summary) != "" {
		fmt.Fprintf(&b, "## Summary\n\n%s\n", strings.TrimSpace(summary))
	}
	return b.String()
}

func discordSummaryURL(meta VoiceTranscriptSummaryMeta) string {
	if meta.GuildID == "" || meta.TranscriptChannelID == "" || meta.SummaryMessageID == "" {
		return ""
	}
	return fmt.Sprintf("https://discord.com/channels/%s/%s/%s", meta.GuildID, meta.TranscriptChannelID, meta.SummaryMessageID)
}

var voiceSummaryWikilinkRE = regexp.MustCompile(`!?\[\[([^\[\]]+)\]\]`)

func summaryWikilinks(summary string) []string {
	matches := voiceSummaryWikilinkRE.FindAllStringSubmatch(summary, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := map[string]bool{}
	links := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		target := strings.TrimSpace(match[1])
		if before, _, ok := strings.Cut(target, "|"); ok {
			target = strings.TrimSpace(before)
		}
		if target == "" || seen[target] {
			continue
		}
		seen[target] = true
		links = append(links, target)
	}
	return links
}

func yamlQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

func slugifyVoiceSummaryPath(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r == '-' || r == '_' || r == ' ':
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
