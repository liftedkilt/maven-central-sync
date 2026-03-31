package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	mavensync "github.com/liftedkilt/maven-central-sync/internal/sync"
)

// SyncJob represents a single artifact sync request.
type SyncJob struct {
	Repository string
	GroupID    string
	ArtifactID string
	Version    string
}

// SyncStatus tracks the state of a sync job.
type SyncStatus struct {
	State       string    `json:"state"`
	Error       string    `json:"error,omitempty"`
	StartedAt   time.Time `json:"startedAt,omitempty"`
	CompletedAt time.Time `json:"completedAt,omitempty"`
}

// Worker processes sync jobs asynchronously with deduplication.
type Worker struct {
	jobs        chan SyncJob
	statuses    sync.Map
	cfg         Config
	concurrency int
}

// gavKey returns the deduplication key for a job.
func gavKey(groupID, artifactID, version string) string {
	return fmt.Sprintf("%s:%s:%s", groupID, artifactID, version)
}

// NewWorker creates and returns a new Worker.
func NewWorker(cfg Config) *Worker {
	return &Worker{
		jobs:        make(chan SyncJob, 100),
		cfg:         cfg,
		concurrency: cfg.WorkerConcurrency,
	}
}

// Start launches the worker goroutines.
func (w *Worker) Start() {
	for i := 0; i < w.concurrency; i++ {
		go func(id int) {
			slog.Info("Worker started", "worker", id)
			for job := range w.jobs {
				w.processJob(job)
			}
		}(i)
	}

	slog.Info("Worker pool started", "concurrency", w.concurrency)
}

// Enqueue adds a job to the queue. Returns "accepted" if new, "duplicate" if already pending/processing.
func (w *Worker) Enqueue(job SyncJob) string {
	key := gavKey(job.GroupID, job.ArtifactID, job.Version)

	if existing, ok := w.statuses.Load(key); ok {
		status := existing.(*SyncStatus)
		if status.State == "pending" || status.State == "processing" {
			slog.Info("Duplicate job, skipping", "gav", key, "state", status.State)
			return "duplicate"
		}
	}

	w.statuses.Store(key, &SyncStatus{State: "pending"})
	w.jobs <- job

	slog.Info("Job enqueued", "gav", key)

	return "accepted"
}

// GetStatus returns the status for a GAV key, or nil if not found.
func (w *Worker) GetStatus(gav string) *SyncStatus {
	if v, ok := w.statuses.Load(gav); ok {
		return v.(*SyncStatus)
	}

	return nil
}

// ListStatuses returns a copy of all statuses.
func (w *Worker) ListStatuses() map[string]*SyncStatus {
	result := make(map[string]*SyncStatus)

	w.statuses.Range(func(key, value any) bool {
		result[key.(string)] = value.(*SyncStatus)
		return true
	})

	return result
}

// processJob does the actual sync work: fetch from Nexus, publish to Maven Central.
func (w *Worker) processJob(job SyncJob) {
	key := gavKey(job.GroupID, job.ArtifactID, job.Version)

	status := &SyncStatus{
		State:     "processing",
		StartedAt: time.Now(),
	}
	w.statuses.Store(key, status)

	slog.Info("Processing job",
		"gav", key,
		"repository", job.Repository,
	)

	client := &http.Client{Timeout: w.cfg.HTTPTimeout}

	// Step 1: Fetch from Nexus
	tempDir, files, err := mavensync.FetchComponentAssets(
		w.cfg.NexusURL, w.cfg.NexusUsername, w.cfg.NexusPassword,
		job.Repository, job.GroupID, job.ArtifactID, job.Version,
		client, w.cfg.FetchRetryTimeout,
	)
	if err != nil {
		status.State = "failed"
		status.Error = fmt.Sprintf("fetch failed: %v", err)
		status.CompletedAt = time.Now()
		w.statuses.Store(key, status)
		slog.Error("Job failed: fetch error", "gav", key, "error", err)
		return
	}

	defer os.RemoveAll(tempDir)

	if len(files) == 0 {
		status.State = "failed"
		status.Error = "no assets found"
		status.CompletedAt = time.Now()
		w.statuses.Store(key, status)
		slog.Error("Job failed: no assets", "gav", key)
		return
	}

	// Step 2: Publish to Maven Central
	result, err := mavensync.Publish(tempDir, w.cfg.MavenCentralURL, w.cfg.MavenCentralToken, client, w.cfg.PublishTimeout, w.cfg.PublishPollInterval)
	if err != nil {
		status.State = "failed"
		status.Error = fmt.Sprintf("publish failed: %v", err)
		status.CompletedAt = time.Now()
		w.statuses.Store(key, status)
		slog.Error("Job failed: publish error", "gav", key, "error", err)
		return
	}

	finalState, _ := result["deploymentState"].(string)

	if finalState != "PUBLISHED" {
		status.State = "failed"
		status.Error = fmt.Sprintf("deployment state: %s", finalState)
		status.CompletedAt = time.Now()
		w.statuses.Store(key, status)
		slog.Error("Job failed: bad deployment state", "gav", key, "state", finalState)
		return
	}

	status.State = "completed"
	status.CompletedAt = time.Now()
	w.statuses.Store(key, status)

	slog.Info("Job completed",
		"gav", key,
		"state", finalState,
		"files", len(files),
	)
}
