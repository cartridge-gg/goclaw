package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

type codexJobTaskStore struct {
	tasks []store.SubagentTaskData
}

func (s *codexJobTaskStore) Create(context.Context, *store.SubagentTaskData) error { return nil }
func (s *codexJobTaskStore) Get(context.Context, uuid.UUID) (*store.SubagentTaskData, error) {
	return nil, nil
}
func (s *codexJobTaskStore) UpdateStatus(context.Context, uuid.UUID, string, *string, int, int64, int64) error {
	return nil
}
func (s *codexJobTaskStore) ListByParent(context.Context, string, string) ([]store.SubagentTaskData, error) {
	return nil, nil
}
func (s *codexJobTaskStore) ListBySession(context.Context, string) ([]store.SubagentTaskData, error) {
	return s.tasks, nil
}
func (s *codexJobTaskStore) ListRunningAcrossTenants(context.Context, int) ([]store.SubagentTaskData, error) {
	return nil, nil
}
func (s *codexJobTaskStore) Archive(context.Context, time.Duration) (int64, error) { return 0, nil }
func (s *codexJobTaskStore) UpdateMetadata(context.Context, uuid.UUID, map[string]any) error {
	return nil
}

func TestCodexJobControl_StatusIsScopedToCurrentThread(t *testing.T) {
	session := "agent:eng:discord-eng:group:thread-1"
	rightID := uuid.New()
	wrongThreadID := uuid.New()
	store := &codexJobTaskStore{tasks: []store.SubagentTaskData{
		{
			BaseModel:     storepkgBase(rightID),
			Status:        "running",
			OriginChannel: strPtr("discord-eng"),
			OriginChatID:  strPtr("thread-1"),
			Metadata: map[string]any{
				"kind":         "impl",
				"control_path": "/data/workspace-eng/worktrees/task/.gillen/jobs/" + rightID.String() + "/control.jsonl",
			},
		},
		{
			BaseModel:     storepkgBase(wrongThreadID),
			Status:        "running",
			OriginChannel: strPtr("discord-eng"),
			OriginChatID:  strPtr("thread-2"),
			Metadata:      map[string]any{"kind": "impl"},
		},
	}}
	ctx := WithToolSessionKey(context.Background(), session)
	ctx = WithToolChannel(ctx, "discord-eng")
	ctx = WithToolChatID(ctx, "thread-1")

	res := NewCodexJobControlTool(store).Execute(ctx, map[string]any{"action": "status"})
	if res == nil || res.IsError {
		t.Fatalf("status returned error: %+v", res)
	}
	if !strings.Contains(res.ForLLM, rightID.String()) {
		t.Fatalf("status did not select current thread job: %s", res.ForLLM)
	}
	if strings.Contains(res.ForLLM, wrongThreadID.String()) {
		t.Fatalf("status leaked wrong thread job: %s", res.ForLLM)
	}
}

func TestCodexJobControl_RejectsJobOutsideCurrentThread(t *testing.T) {
	otherID := uuid.New()
	store := &codexJobTaskStore{tasks: []store.SubagentTaskData{{
		BaseModel:     storepkgBase(otherID),
		Status:        "running",
		OriginChannel: strPtr("discord-eng"),
		OriginChatID:  strPtr("thread-2"),
		Metadata:      map[string]any{"kind": "impl"},
	}}}
	ctx := WithToolSessionKey(context.Background(), "agent:eng:discord-eng:group:thread-1")
	ctx = WithToolChannel(ctx, "discord-eng")
	ctx = WithToolChatID(ctx, "thread-1")

	res := NewCodexJobControlTool(store).Execute(ctx, map[string]any{"action": "status", "job_id": otherID.String()})
	if res == nil || !res.IsError {
		t.Fatalf("expected scoped rejection, got %+v", res)
	}
	if !strings.Contains(res.ForLLM, "not attached to this thread") {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
}

func storepkgBase(id uuid.UUID) store.BaseModel {
	return store.BaseModel{ID: id}
}
