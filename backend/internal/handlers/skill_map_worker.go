package handlers

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/dreamtrans/backend/internal/deployment"

	"github.com/dreamtrans/backend/internal/config"
	"github.com/dreamtrans/backend/internal/metrics"
	"github.com/dreamtrans/backend/internal/models"
	"github.com/dreamtrans/backend/internal/rag"
	"github.com/dreamtrans/backend/internal/store"
	"github.com/google/uuid"
)

const (
	defaultSkillMapWorkers = 1
	maxSkillMapWorkers     = 8
	skillMapJobLease       = 3 * time.Minute
	skillMapJobLeaseRenew  = 45 * time.Second
	skillMapJobTimeout     = 30 * time.Minute
	skillMapJobPoll        = 3 * time.Second
)

type skillMapJobPool struct {
	handler  *RAGHandler
	workerID string
	wake     chan struct{}
	stop     chan struct{}
	once     sync.Once
	active   sync.Map
	wg       sync.WaitGroup
}

type skillMapActiveRun struct {
	cancel context.CancelFunc
}

var skillMapJobPools sync.Map

func skillMapWorkerCount() int {
	return int(envInteger(
		"SKILL_MAP_WORKERS", defaultSkillMapWorkers, 1, maxSkillMapWorkers,
	))
}

func (h *RAGHandler) resumeSkillMapJobs() {
	if h.store == nil || h.svc == nil {
		return
	}
	workers := skillMapWorkerCount()
	pool := &skillMapJobPool{
		handler: h, workerID: "skill-map-" + uuid.NewString(),
		wake: make(chan struct{}, workers), stop: make(chan struct{}),
	}
	actual, loaded := skillMapJobPools.LoadOrStore(h, pool)
	if loaded {
		actual.(*skillMapJobPool).signal()
		return
	}
	for range workers {
		pool.wg.Add(1)
		go pool.worker()
	}
}

func (h *RAGHandler) stopSkillMapJobs() {
	value, ok := skillMapJobPools.LoadAndDelete(h)
	if !ok {
		return
	}
	pool := value.(*skillMapJobPool)
	pool.once.Do(func() { close(pool.stop) })
	pool.active.Range(func(_, value any) bool {
		value.(*skillMapActiveRun).cancel()
		return true
	})
	pool.wg.Wait()
}

func (h *RAGHandler) signalSkillMapJobs() {
	value, ok := skillMapJobPools.Load(h)
	if !ok {
		h.resumeSkillMapJobs()
		value, ok = skillMapJobPools.Load(h)
	}
	if ok {
		value.(*skillMapJobPool).signal()
	}
}

func (p *skillMapJobPool) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *skillMapJobPool) worker() {
	defer p.wg.Done()
	ticker := time.NewTicker(skillMapJobPoll)
	defer ticker.Stop()
	for {
		if p.claimAndRun() {
			continue
		}
		select {
		case <-p.stop:
			return
		case <-p.wake:
		case <-ticker.C:
		}
	}
}

func (p *skillMapJobPool) claimAndRun() bool {
	done, allowed := deployment.Default.BeginTask()
	if !allowed {
		return false
	}
	defer done()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	leaseOwner := p.workerID + "-" + uuid.NewString()
	jobs, err := p.handler.store.ClaimSkillMapJobs(ctx, leaseOwner, 1, skillMapJobLease)
	cancel()
	if err != nil {
		log.Printf("claim skill map job: %v", err)
		return false
	}
	if len(jobs) == 0 {
		return false
	}
	p.run(&jobs[0])
	return true
}

func (p *skillMapJobPool) run(job *models.SkillMapJob) {
	ctx, cancel := context.WithTimeout(context.Background(), skillMapJobTimeout)
	p.active.Store(job.ID, &skillMapActiveRun{cancel: cancel})
	defer func() {
		p.active.Delete(job.ID)
		cancel()
	}()
	renewDone := make(chan struct{})
	go p.renewLease(ctx, cancel, renewDone, job.ID, job.LeaseOwner)

	runErr := p.handler.processSkillMapJob(ctx, job)
	close(renewDone)

	finalCtx, finalCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer finalCancel()
	if runErr == nil {
		if completeErr := p.handler.store.CompleteSkillMapJob(
			finalCtx, job.ID, job.LeaseOwner, job.CostUSD,
		); completeErr != nil && !errors.Is(completeErr, store.ErrLeaseLost) {
			log.Printf("complete skill map job %s: %v", job.ID, completeErr)
		}
	} else if !errors.Is(runErr, store.ErrLeaseLost) &&
		!errors.Is(runErr, context.Canceled) {
		retryable := isRetryableSkillMapError(runErr)
		if failErr := p.handler.store.FailSkillMapJob(
			finalCtx, job.ID, job.LeaseOwner, safeSkillMapError(runErr), retryable,
		); failErr != nil && !errors.Is(failErr, store.ErrLeaseLost) {
			log.Printf("fail skill map job %s: %v", job.ID, failErr)
		}
		log.Printf("skill map job %s failed: %v", job.ID, runErr)
	}
	p.signal()
}

func (p *skillMapJobPool) renewLease(
	ctx context.Context, cancel context.CancelFunc, done <-chan struct{},
	jobID, leaseOwner string,
) {
	ticker := time.NewTicker(skillMapJobLeaseRenew)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			renewCtx, renewCancel := context.WithTimeout(context.Background(), 5*time.Second)
			ok, err := p.handler.store.RenewSkillMapJobLease(
				renewCtx, jobID, leaseOwner, skillMapJobLease,
			)
			renewCancel()
			if err != nil || !ok {
				cancel()
				return
			}
		}
	}
}

func (h *RAGHandler) processSkillMapJob(
	ctx context.Context, job *models.SkillMapJob,
) error {
	project, err := h.store.GetAIProject(ctx, job.ProjectID, job.UserID)
	if err != nil {
		return err
	}
	if project == nil {
		return errors.New("course not found")
	}
	work, err := h.prepareSkillMapWork(ctx, project, job.ReasoningEffort, job.Model)
	if err != nil {
		return err
	}
	if err := h.store.UpdateSkillMapJobProgress(
		ctx, job.ID, job.LeaseOwner, 0, len(work.chunks),
	); err != nil {
		return err
	}
	projectID := job.ProjectID
	meter := &ragHTTPUsageMeter{
		billing:         h.billing,
		userID:          job.UserID,
		tenantID:        job.TenantID,
		stableNamespace: "skill-map-job:" + job.ID,
		feature:         studyFeatureSkillMap,
		projectID:       &projectID,
	}
	ctx = rag.WithProviderUsageMeter(ctx, meter)
	overrides := &rag.ChatOverrides{
		Model:           job.Model,
		ReasoningEffort: job.ReasoningEffort,
	}
	if overrides.Model == "" {
		overrides.Model = config.Get().Models.Summary
	}
	rawMap, usage, duration, err := h.generateSkillMapFromChunks(
		ctx, nil, project.ID, job.RequestHash, work.instruction,
		work.chunks, work.previousDoc, overrides,
		func(done, total int) {
			progressCtx, progressCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer progressCancel()
			if progressErr := h.store.UpdateSkillMapJobProgress(
				progressCtx, job.ID, job.LeaseOwner, done, total,
			); progressErr != nil && !errors.Is(progressErr, store.ErrLeaseLost) {
				log.Printf("skill map job %s progress: %v", job.ID, progressErr)
			}
		},
	)
	if err != nil {
		return err
	}
	if usage != nil {
		metrics.RecordSummarize(&metrics.Usage{
			PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
			TotalTokens: usage.TotalTokens, CachedTokens: usage.CachedTokens,
			CacheWriteTokens: usage.CacheWriteTokens, Model: usage.Model,
		}, duration.Milliseconds())
	}
	if err := h.persistGeneratedSkillMap(ctx, project, job, work, rawMap, usage); err != nil {
		return err
	}
	job.CostUSD = meter.ChargedUSD()
	return nil
}

func isRetryableSkillMapError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, store.ErrLeaseLost) {
		return false
	}
	if errors.Is(err, errSkillMapNoSessions) || errors.Is(err, errSkillMapNoTranscripts) ||
		errors.Is(err, errSkillMapInvalidJSON) || errors.Is(err, store.ErrStorageQuota) {
		return false
	}
	return true
}

func safeSkillMapError(err error) string {
	if err == nil {
		return "skill map generation failed"
	}
	message := err.Error()
	if len(message) > 500 {
		return message[:500]
	}
	return message
}
