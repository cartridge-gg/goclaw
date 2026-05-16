package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// CodexJobControlTool lets the inline Discord agent inspect or steer the
// Codex app-server that backs the active Kubernetes job for this thread.
// Scope is intentionally narrow: the target job must belong to the current
// session/channel/thread recorded in subagent_tasks.
type CodexJobControlTool struct {
	taskStore store.SubagentTaskStore
}

func NewCodexJobControlTool(taskStore store.SubagentTaskStore) *CodexJobControlTool {
	return &CodexJobControlTool{taskStore: taskStore}
}

func (t *CodexJobControlTool) Name() string { return "codex_job_control" }

func (t *CodexJobControlTool) Description() string {
	return "Inspect or steer the active Codex-backed Kubernetes job for the current Discord thread. Supports status, steer, interrupt, and scoped raw JSON-RPC queueing."
}

func (t *CodexJobControlTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"status", "steer", "interrupt", "rpc"},
				"description": "status returns the current task metadata; steer sends a user message into the active Codex turn; interrupt cancels the active turn; rpc queues a raw Codex JSON-RPC method.",
			},
			"job_id": map[string]any{
				"type":        "string",
				"description": "Optional job UUID. Defaults to the latest running job for this Discord thread.",
			},
			"message": map[string]any{
				"type":        "string",
				"description": "Message for action=steer.",
			},
			"method": map[string]any{
				"type":        "string",
				"description": "Codex JSON-RPC method for action=rpc, such as turn/steer or turn/interrupt.",
			},
			"params": map[string]any{
				"type":        "object",
				"description": "Raw Codex JSON-RPC params for action=rpc.",
			},
		},
		"required": []string{"action"},
	}
}

func (t *CodexJobControlTool) Execute(ctx context.Context, args map[string]any) *Result {
	if t.taskStore == nil {
		return ErrorResult("codex_job_control: task store is not configured")
	}
	task, err := t.resolveTask(ctx, strings.TrimSpace(anyString(args["job_id"])))
	if err != nil {
		return ErrorResult("codex_job_control: " + err.Error())
	}

	action := strings.TrimSpace(anyString(args["action"]))
	switch action {
	case "status":
		payload := map[string]any{
			"job_id":   task.ID.String(),
			"status":   task.Status,
			"metadata": task.Metadata,
		}
		raw, _ := json.Marshal(payload)
		return SilentResult(string(raw))
	case "steer":
		msg := strings.TrimSpace(anyString(args["message"]))
		if msg == "" {
			return ErrorResult("codex_job_control: message is required for steer")
		}
		return t.enqueue(task, map[string]any{"action": "steer", "message": msg})
	case "interrupt":
		return t.enqueue(task, map[string]any{"action": "interrupt"})
	case "rpc":
		method := strings.TrimSpace(anyString(args["method"]))
		if method == "" {
			return ErrorResult("codex_job_control: method is required for rpc")
		}
		params, _ := args["params"].(map[string]any)
		return t.enqueue(task, map[string]any{"action": "rpc", "method": method, "params": params})
	default:
		return ErrorResult("codex_job_control: unsupported action " + action)
	}
}

func (t *CodexJobControlTool) resolveTask(ctx context.Context, jobID string) (*store.SubagentTaskData, error) {
	sessionKey := ToolSessionKeyFromCtx(ctx)
	if sessionKey == "" {
		return nil, fmt.Errorf("no current session key")
	}
	tasks, err := t.taskStore.ListBySession(ctx, sessionKey)
	if err != nil {
		return nil, err
	}
	channel := ToolChannelFromCtx(ctx)
	chatID := ToolChatIDFromCtx(ctx)
	var selected *store.SubagentTaskData
	for i := range tasks {
		task := &tasks[i]
		if jobID != "" && task.ID.String() != jobID {
			continue
		}
		if channel != "" && (task.OriginChannel == nil || *task.OriginChannel != channel) {
			continue
		}
		if chatID != "" && (task.OriginChatID == nil || *task.OriginChatID != chatID) {
			continue
		}
		if selected == nil || task.Status == "running" {
			selected = task
		}
		if task.Status == "running" {
			break
		}
	}
	if selected == nil {
		if jobID != "" {
			return nil, fmt.Errorf("job %s is not attached to this thread", jobID)
		}
		return nil, fmt.Errorf("no job attached to this thread")
	}
	return selected, nil
}

func (t *CodexJobControlTool) enqueue(task *store.SubagentTaskData, payload map[string]any) *Result {
	if task.Status != "running" {
		return ErrorResult("codex_job_control: job is not running")
	}
	controlPath := metadataString(task.Metadata, "control_path")
	if controlPath == "" {
		return ErrorResult("codex_job_control: job has no control path")
	}
	if !strings.HasPrefix(filepath.Clean(controlPath), "/data/") {
		return ErrorResult("codex_job_control: refusing control path outside /data")
	}
	if err := os.MkdirAll(filepath.Dir(controlPath), 0o755); err != nil {
		return ErrorResult("codex_job_control: create control dir: " + err.Error())
	}
	payload["request_id"] = uuid.NewString()
	payload["created_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	payload["job_id"] = task.ID.String()
	raw, err := json.Marshal(payload)
	if err != nil {
		return ErrorResult("codex_job_control: marshal request: " + err.Error())
	}
	f, err := os.OpenFile(controlPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return ErrorResult("codex_job_control: open control path: " + err.Error())
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return ErrorResult("codex_job_control: write control request: " + err.Error())
	}
	return SilentResult(fmt.Sprintf(`{"status":"queued","job_id":%q,"request_id":%q}`, task.ID.String(), payload["request_id"]))
}

func metadataString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	return strings.TrimSpace(anyString(m[key]))
}
