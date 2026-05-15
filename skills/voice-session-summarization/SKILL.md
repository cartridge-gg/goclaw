---
name: voice-session-summarization
description: Summarize a Discord voice session transcript using semantic memory context (contributor identities, project terminology, recent prior sessions) from the agent's memory vault. Project-agnostic — works for any Obsidian-style vault.
---

# Voice Session Summarization

You are summarizing a Discord voice channel conversation. The user message contains session metadata plus a transcript with each line in the form `<speaker name>: <what they said>`. Your job is to produce a useful summary whose depth scales with the call duration and content density. The returned text is written to an Obsidian-compatible memory note, then rendered into Discord with wikilink brackets removed, exact speaker names converted to Discord mentions, and action items rendered as Discord embeds.

## Required steps

Relevant memory snippets are injected into the system prompt before this task. You do not have live tools here; use only the transcript and injected memory context.

### 1. Speaker identity resolution

For each unique speaker prefix (`<DisplayName>:`), inspect the injected memory context for contributor / person pages that resolve the display name.

If a clearly canonical contributor page appears, use an Obsidian wikilink in the memory-oriented summary, but keep the display alias as the exact transcript speaker prefix so Discord mention conversion can still work. Example: `[[Mateusz Chudkowski|mataleone]]`.

### 2. Topic identification

Identify product / project / component nouns in the transcript. If the injected memory context includes a definition page (frontmatter `type` in `project | product | component | concept | theme`), reference it via wikilink `[[Page Title]]` in the summary. This gives the persisted memory note real cross-references back to the wiki, which Obsidian renders as backlinks for free.

### 3. Temporal context

Use injected snippets for recent prior voice-session entries, project rollups, and contributor pages. When the current call references a thread that was discussed before, mention it briefly ("continuing last week's [[Glitch Bomb]] discussion ...").

### 4. Compose

- Scale detail to the call: short/content-light calls can be 2-4 bullets or paragraphs; 10-30 minute calls should usually be 4-8 bullets or paragraphs; 30+ minute calls should usually be 8-14 bullets or paragraphs with meaningful decisions, tradeoffs, unresolved questions, and context.
- <=3500 characters total.
- Lead with the topic / decision, not generic preamble.
- Use canonical names + wikilinks per steps 1-2.
- Quote at most one short, distinctive line if it's load-bearing.
- Capture concrete tasks, owners, assignments, follow-ups, and proposed next steps.
- Skip filler ("uh", "you know"), greetings, side-channel chatter.
- If the conversation was very short or content-free, just say so in one sentence.
- No Markdown headings; small inline emphasis (italics, bold) and Obsidian wikilinks are fine.

### 4.1 Action items

If tasks, owners, assignments, follow-ups, or proposed next steps were discussed, end with a section headed exactly `Action items:`. Format each task as:

`- Owner: task`

Use the exact transcript speaker prefix as `Owner` when a task is assigned to a person, so Discord can convert it into a mention. Use `Unassigned` when no owner is clear. Keep uncertainty in the task text rather than inventing ownership, for example `- Unassigned: Decide whether to include PvP in the MVP scope.`

### 5. Persist (optional — only when configured)

GoClaw persists the returned summary automatically when `session_output_dir` is configured. Do not write files yourself. The persisted file includes frontmatter for channel, timestamps, Discord source URL, participants, duration, utterance count, and tags.

### 6. Return

Return only the summary body text. Do not include frontmatter. The raw text becomes the memory note body; Discord receives a cleaned rendering with `[[...]]` brackets removed, exact speaker references converted to mentions, and the `Action items:` section rendered as task embeds.

## Project-agnostic design

This skill makes no assumptions about a specific vault layout. It uses semantic `memory_search` to discover contributor pages, project pages, and prior session entries — so the same skill runs unchanged against any team's vault that uses Obsidian frontmatter + wikilink conventions. Project-specific knowledge (the actual contributor names, the actual product taxonomy) lives in the vault content, not in this skill.
