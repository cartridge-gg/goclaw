package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

func TestCronHandlerCreatePersistsOverrides(t *testing.T) {
	fakeStore := newFakeCronStore()
	handler := NewCronHandler(fakeStore)

	req := httptest.NewRequest(http.MethodPost, "/v1/cron", strings.NewReader(`{
		"name":"daily-social-rollup",
		"schedule":{"kind":"cron","expr":"0 9 * * *","tz":"UTC"},
		"message":"Summarize social metrics",
		"enabled":false,
		"managed":{"by":"gillen","source":"configmap","key":"social-rollup","version":"v1","declaredHash":"abc"},
		"provider":"openai",
		"model":"gpt-5.5"
	}`))
	rec := httptest.NewRecorder()

	handler.handleCreate(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}
	job := fakeStore.jobs["job-1"]
	if job == nil {
		t.Fatal("expected job to be created")
	}
	if job.Enabled {
		t.Fatal("expected enabled override to be persisted")
	}
	if job.Managed.By != "gillen" || job.Managed.Key != "social-rollup" {
		t.Fatalf("managed metadata not persisted: %+v", job.Managed)
	}
	if job.Provider != "openai" || job.Model != "gpt-5.5" {
		t.Fatalf("provider/model not persisted: provider=%q model=%q", job.Provider, job.Model)
	}
}

func TestCronHandlerListFiltersManagedJobs(t *testing.T) {
	fakeStore := newFakeCronStore()
	fakeStore.jobs["a"] = fakeJob("a", "gillen", "one")
	fakeStore.jobs["b"] = fakeJob("b", "other", "two")
	handler := NewCronHandler(fakeStore)

	req := httptest.NewRequest(http.MethodGet, "/v1/cron?includeDisabled=true&managedBy=gillen&managedKey=one", nil)
	rec := httptest.NewRecorder()

	handler.handleList(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Jobs []store.CronJob `json:"jobs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Jobs) != 1 || resp.Jobs[0].ID != "a" {
		t.Fatalf("unexpected jobs: %+v", resp.Jobs)
	}
}

func fakeJob(id, by, key string) *store.CronJob {
	return &store.CronJob{
		ID:      id,
		Name:    id,
		Enabled: true,
		Managed: store.CronManaged{By: by, Key: key},
		Schedule: store.CronSchedule{
			Kind: "cron",
			Expr: "0 9 * * *",
		},
		Payload: store.CronPayload{Kind: "agent_turn", Message: "run"},
	}
}

type fakeCronStore struct {
	jobs  map[string]*store.CronJob
	onJob func(job *store.CronJob) (*store.CronJobResult, error)
}

func newFakeCronStore() *fakeCronStore {
	return &fakeCronStore{jobs: make(map[string]*store.CronJob)}
}

func (s *fakeCronStore) AddJob(_ context.Context, name string, schedule store.CronSchedule, message string, deliver bool, channel, to, agentID, userID string) (*store.CronJob, error) {
	job := store.CronJob{
		ID:             "job-1",
		Name:           name,
		AgentID:        agentID,
		UserID:         userID,
		Enabled:        true,
		Schedule:       schedule,
		Payload:        store.CronPayload{Kind: "agent_turn", Message: message},
		Deliver:        deliver,
		DeliverChannel: channel,
		DeliverTo:      to,
	}
	s.jobs[job.ID] = &job
	return &job, nil
}

func (s *fakeCronStore) GetJob(_ context.Context, jobID string) (*store.CronJob, bool) {
	job, ok := s.jobs[jobID]
	return job, ok
}

func (s *fakeCronStore) ListJobs(_ context.Context, includeDisabled bool, agentID, userID string) []store.CronJob {
	var jobs []store.CronJob
	for _, job := range s.jobs {
		if !includeDisabled && !job.Enabled {
			continue
		}
		if agentID != "" && job.AgentID != agentID {
			continue
		}
		if userID != "" && job.UserID != userID {
			continue
		}
		jobs = append(jobs, *job)
	}
	return jobs
}

func (s *fakeCronStore) RemoveJob(_ context.Context, jobID string) error {
	delete(s.jobs, jobID)
	return nil
}

func (s *fakeCronStore) UpdateJob(_ context.Context, jobID string, patch store.CronJobPatch) (*store.CronJob, error) {
	job := s.jobs[jobID]
	if job == nil {
		return nil, store.ErrCronJobNotFound
	}
	if patch.Enabled != nil {
		job.Enabled = *patch.Enabled
	}
	if patch.Managed != nil {
		job.Managed = *patch.Managed
	}
	if patch.Provider != nil {
		job.Provider = *patch.Provider
	}
	if patch.Model != nil {
		job.Model = *patch.Model
	}
	if patch.Stateless != nil {
		job.Stateless = *patch.Stateless
	}
	if patch.WakeHeartbeat != nil {
		job.WakeHeartbeat = *patch.WakeHeartbeat
	}
	if patch.DeleteAfterRun != nil {
		job.DeleteAfterRun = *patch.DeleteAfterRun
	}
	return job, nil
}

func (s *fakeCronStore) EnableJob(_ context.Context, jobID string, enabled bool) error {
	job := s.jobs[jobID]
	if job == nil {
		return store.ErrCronJobNotFound
	}
	job.Enabled = enabled
	return nil
}

func (s *fakeCronStore) GetRunLog(context.Context, string, int, int) ([]store.CronRunLogEntry, int) {
	return nil, 0
}

func (s *fakeCronStore) Status() map[string]any { return map[string]any{"jobs": len(s.jobs)} }
func (s *fakeCronStore) Start() error           { return nil }
func (s *fakeCronStore) Stop()                  {}
func (s *fakeCronStore) SetOnJob(handler func(job *store.CronJob) (*store.CronJobResult, error)) {
	s.onJob = handler
}
func (s *fakeCronStore) SetOnEvent(func(event store.CronEvent)) {}
func (s *fakeCronStore) RunJob(context.Context, string, bool) (bool, string, error) {
	return true, "ran", nil
}
func (s *fakeCronStore) SetDefaultTimezone(string) {}
func (s *fakeCronStore) GetDueJobs(time.Time) []store.CronJob {
	return nil
}
