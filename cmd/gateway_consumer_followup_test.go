package cmd

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateForReminder_TruncatesByRuneAndKeepsUTF8Valid(t *testing.T) {
	input := strings.Repeat("a", 199) + "✌️"

	got := truncateForReminder(input, 200)
	if !utf8.ValidString(got) {
		t.Fatalf("truncateForReminder() produced invalid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("truncateForReminder() = %q, want suffix ...", got)
	}
	if strings.Contains(got, "\uFFFD") {
		t.Fatalf("truncateForReminder() introduced replacement rune: %q", got)
	}
}

func TestTruncateForReminder_StripsInvalidUTF8(t *testing.T) {
	invalid := string([]byte{'a', 0xef, 0xb8, '.', 'b'})

	got := truncateForReminder(invalid, 200)
	if !utf8.ValidString(got) {
		t.Fatalf("truncateForReminder() produced invalid UTF-8: %q", got)
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("truncateForReminder() = %q, want no replacement rune", got)
	}
}

func TestBuildTrustedComponentPrompt(t *testing.T) {
	got := buildTrustedComponentPrompt(map[string]string{
		"interaction_kind":         "component",
		"component_type":           "button",
		"button_custom_id":         "billing:approve:5cba99db1cbdabfb",
		"component_parent_channel": "1012373554811113493",
		"component_parent_message": "1503479295497076736",
		"component_parent_content": "ignored parent content",
		"channel_id":               "1012373554811113493",
		"guild_id":                 "954866867376357397",
		"user_id":                  "989554992786579536",
		"message_id":               "1503586590478569472",
	})

	for _, want := range []string{
		"This wake came from a platform component interaction, not typed text.",
		"button_custom_id: billing:approve:5cba99db1cbdabfb",
		"component_parent_channel: 1012373554811113493",
		"component_parent_message: 1503479295497076736",
		"[/Trusted platform event metadata]",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("buildTrustedComponentPrompt() missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ignored parent content") {
		t.Fatalf("buildTrustedComponentPrompt() included parent content: %s", got)
	}
}

func TestBuildTrustedComponentPromptIgnoresNonComponent(t *testing.T) {
	if got := buildTrustedComponentPrompt(map[string]string{"button_custom_id": "billing:approve:5cba99db1cbdabfb"}); got != "" {
		t.Fatalf("buildTrustedComponentPrompt() = %q, want empty", got)
	}
}
