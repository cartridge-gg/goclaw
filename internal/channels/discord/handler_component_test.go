package discord

import (
	"testing"

	"github.com/cartridge-gg/discordgo"
)

func TestComponentInteractionACKDoesNotRequireFollowup(t *testing.T) {
	ack := componentInteractionACK()
	if ack == nil {
		t.Fatal("componentInteractionACK returned nil")
	}
	if ack.Type != discordgo.InteractionResponseDeferredMessageUpdate {
		t.Fatalf("ack type = %v, want DeferredMessageUpdate", ack.Type)
	}
}

func TestBuildComponentInteractionMetadataOmitsInteractionReplyToken(t *testing.T) {
	parent := &discordgo.Message{
		ID:        "parent-1",
		ChannelID: "channel-1",
		Content:   "approval card",
	}

	got := buildComponentInteractionMetadata(
		"interaction-1",
		"user-1",
		"User#0001",
		"guild-1",
		"channel-1",
		false,
		"billing:approve:5cba99db1cbdabfb",
		parent,
	)

	if got["interaction_kind"] != "component" || got["component_type"] != "button" {
		t.Fatalf("component metadata missing kind/type: %#v", got)
	}
	if got["button_custom_id"] != "billing:approve:5cba99db1cbdabfb" {
		t.Fatalf("button_custom_id = %q", got["button_custom_id"])
	}
	if got["component_parent_message"] != "parent-1" ||
		got["component_parent_channel"] != "channel-1" ||
		got["component_parent_content"] != "approval card" {
		t.Fatalf("parent metadata mismatch: %#v", got)
	}
	for _, forbidden := range []string{
		"discord_interaction_token",
		"discord_interaction_id",
		"discord_interaction_appid",
	} {
		if _, ok := got[forbidden]; ok {
			t.Fatalf("component metadata must not carry %s: %#v", forbidden, got)
		}
	}
}
