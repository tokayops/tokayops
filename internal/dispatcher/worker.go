package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/dispatcher/builders"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/google/uuid"
)

// NotificationTarget identifies where a notification goes, in provider-agnostic
// terms. ID is the provider-specific identifier (e.g. a Slack channel ID, or a
// resolved Slack user ID for a DM).
type NotificationTarget struct {
	Kind string // "channel" | "user"
	ID   string
}

// NotificationRequest is a typed send request covering both alert cards (have an
// AlertGroup, no free-form Message) and free-form DMs/OTP/handoff (have a Message,
// no AlertGroup). Providers MUST decide behaviour from Target.Kind, Editable,
// AlertGroup and Message — NOT from Kind, which is for metrics/context only.
type NotificationRequest struct {
	Kind       string // step kind for metrics/context: slack_channel|slack_dm|firehose|otp|handoff
	Target     NotificationTarget
	Message    string            // free-form text (DM/OTP/handoff); empty for alert cards
	AlertGroup *model.AlertGroup // optional; present for alert cards
	Editable   bool              // true => updatable card (returns payload); false => fire-and-forget
}

// Provider interface defines notification provider operations.
//
// Send returns an OPTIONAL provider payload that is persisted to
// notification_deliveries.provider_payload and later handed back to Update/Resolve.
// For fire-and-forget sends (Editable=false) an empty payload is valid.
type Provider interface {
	Send(ctx context.Context, req NotificationRequest) (string, error)
	Update(ctx context.Context, d *model.NotificationDelivery, ag *model.AlertGroup) (string, error)
	Resolve(ctx context.Context, d *model.NotificationDelivery, ag *model.AlertGroup) error
	Permalink(d *model.NotificationDelivery) string
}

type Dispatcher struct {
	store     store.StoreInterface
	cfg       *config.Config
	providers *ProviderRegistry
	// WorkerID is a per-process identity used only for log correlation. It is
	// NOT the lease token: ClaimNextJobSteps generates locked_by in the DB
	// (gen_random_uuid) and returns it as step.LockedBy — that value is what the
	// owned-step CAS calls (UpdateJobStepIfOwned / ExtendStepLease /
	// FinishStepAndAdvance) carry as leaseToken.
	WorkerID  string
	executors map[string]StepExecutor
}

func NewDispatcher(s store.StoreInterface, cfg *config.Config) (*Dispatcher, error) {
	if s == nil {
		return nil, errors.New("dispatcher requires store")
	}
	if cfg == nil {
		cfg = &config.Config{}
	}

	d := &Dispatcher{
		store:     s,
		cfg:       cfg,
		providers: NewProviderRegistry(s),
		executors: make(map[string]StepExecutor),
		WorkerID:  uuid.New().String(),
	}

	escalationExec := NewEscalationExecutor(s, d.providers, cfg)

	// Step types are (provider, target_kind) pairs; the executor switches on
	// target_kind. "firehose" stays as a sentinel because it is intentionally
	// Slack-only (bypasses the taxonomy — see escalation_executor.go).
	d.RegisterExecutor("dm", escalationExec)
	d.RegisterExecutor("channel", escalationExec)
	d.RegisterExecutor("firehose", escalationExec)

	resolutionExec := NewResolutionExecutor(s, d.providers, cfg)
	d.RegisterExecutor("resolve", resolutionExec)
	d.RegisterExecutor("update", resolutionExec) // update uses same executor with Operation field

	handoffExec := NewHandoffExecutor(d.providers)
	d.RegisterExecutor("handoff_notify", handoffExec)

	return d, nil
}

// RegisterProvider registers a fixed Provider instance under a name (used by tests and
// simple wiring). Production code that needs per-integration resolution uses
// RegisterProviderFactory instead.
func (d *Dispatcher) RegisterProvider(name string, p Provider) {
	d.providers.RegisterStatic(name, p)
}

// RegisterProviderFactory registers a factory that builds a Provider bound to the
// enabled integration of integType, keyed by integration ID.
func (d *Dispatcher) RegisterProviderFactory(name string, integType model.IntegrationType, build providerFactory) {
	d.providers.RegisterFactory(name, integType, build)
}

// RegisterProviderCapabilities is a thin pass-through used at startup to
// declare what a provider can do (target kinds, integration type). Read by
// API / UI for the policy editor; not used for runtime resolution.
func (d *Dispatcher) RegisterProviderCapabilities(c ProviderCapabilities) {
	d.providers.RegisterCapabilities(c)
}

// Providers exposes the registry so the API layer can read capabilities.
// Callers must treat the result as read-only — runtime resolution is the
// dispatcher's concern.
func (d *Dispatcher) Providers() *ProviderRegistry {
	return d.providers
}

func (d *Dispatcher) RegisterExecutor(stepType string, e StepExecutor) {
	d.executors[stepType] = e
}

func (d *Dispatcher) Run(ctx context.Context) {
	log.Printf("StepWorker (WorkerID: %s) started", d.WorkerID)
	go d.jobProcessingLoop(ctx)
	go d.ackUpdateProcessingLoop(ctx)
	go d.resolutionProcessingLoop(ctx)
	go d.alertUpdateProcessingLoop(ctx)
	<-ctx.Done()
}

// =================================================================================
// Job Processing Loop (New Architecture)
// =================================================================================

func (d *Dispatcher) jobProcessingLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.ProcessPendingSteps(ctx)
		}
	}
}

func (d *Dispatcher) ProcessPendingSteps(ctx context.Context) {
	steps, err := d.store.ClaimNextJobSteps(10, 60*time.Second)
	if err != nil {
		log.Printf("StepWorker: Failed to claim steps: %v", err)
		return
	}

	for _, step := range steps {
		go d.processStep(ctx, step)
	}
}

func (d *Dispatcher) processStep(ctx context.Context, step *model.JobStep) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("StepWorker: PANIC in processStep: %v", r)
			d.failStep(step, fmt.Sprintf("panic: %v", r))
		}
	}()

	// 1. Check Job Status (ensure not canceled)
	job, err := d.store.GetJobByID(step.JobID)
	if err != nil {
		log.Printf("StepWorker: Job %s lookup failed: %v", step.JobID, err)
		d.handleStepRetry(step, err)
		return
	}
	if job == nil {
		log.Printf("StepWorker: Job %s not found (nil)", step.JobID)
		d.failStep(step, "Job not found")
		return
	}

	// If Job is canceled/failed/paused -> we shouldn't run this step.
	if job.Status != model.JobStatusRunning && job.Status != model.JobStatusPending {
		log.Printf("StepWorker: Job %s is not running (status: %s), canceling step %d", job.ID, job.Status, step.StepIndex)
		step.Status = model.JobStepStatusCanceled
		step.Error = strPtr(fmt.Sprintf("Job status is %s", job.Status))
		step.LockedUntil = nil
		if step.LockedBy != nil {
			d.store.UpdateJobStepIfOwned(step, *step.LockedBy)
		}
		return
	}

	// 2. Lookup Executor
	executor, ok := d.executors[step.StepType]
	if !ok {
		msg := fmt.Sprintf("unknown step type: %s", step.StepType)
		log.Printf("StepWorker: %s", msg)
		d.failStep(step, msg)
		return
	}

	log.Printf("StepWorker: Job %s Step %d exec -> %s", step.JobID, step.StepIndex, step.StepType)

	// 3. Execute Step
	resultData, execErr := executor.Execute(ctx, job, step)

	// 4. Handle Result
	if execErr != nil {
		metrics.JobStepsProcessedTotal.WithLabelValues(step.StepType, "failed").Inc()
		// Strict Retry Policy
		if step.MaxAttempts <= 1 {
			log.Printf("StepWorker: Step %s failed (MaxAttempts=%d), failing immediately: %v", step.ID, step.MaxAttempts, execErr)
			d.failStep(step, execErr.Error())
		} else {
			d.handleStepRetry(step, execErr)
		}
	} else {
		metrics.JobStepsProcessedTotal.WithLabelValues(step.StepType, "succeeded").Inc()
		d.completeStep(step, resultData)
	}
}

func (d *Dispatcher) handleStepRetry(step *model.JobStep, err error) {
	// Check for permanent errors that shouldn't be retried
	if isPermanentError(err) {
		log.Printf("StepWorker: Step %s failed with permanent error (no retry): %v", step.ID, err)
		d.failStep(step, err.Error())
		return
	}

	step.AttemptCount++
	max := step.MaxAttempts
	if max <= 0 {
		max = 5
	}

	if step.AttemptCount >= max {
		// Max retries exhausted — finalize through FinishStepAndAdvance
		log.Printf("StepWorker: Step %s failed permanently: %v. Finishing step.", step.ID, err)
		d.failStep(step, err.Error())
	} else {
		// Schedule retry via lease-checked update
		step.Status = model.JobStepStatusRetry
		step.Error = strPtr(err.Error())
		step.LockedUntil = nil

		delay := 30 * time.Second * time.Duration(step.AttemptCount)
		next := time.Now().Add(delay)
		step.NextRunAt = &next

		if step.LockedBy != nil {
			owned, updateErr := d.store.UpdateJobStepIfOwned(step, *step.LockedBy)
			if updateErr != nil {
				log.Printf("StepWorker: Failed to update retry step %s: %v", step.ID, updateErr)
			} else if !owned {
				log.Printf("StepWorker: Lease lost for step %s, skipping retry", step.ID)
				return
			}
		}
		log.Printf("StepWorker: Step %s scheduling retry %d/%d in %v: %v", step.ID, step.AttemptCount, max, delay, err)
	}
}

// isPermanentError checks if the error should not be retried. These are config or
// programming faults (missing token, unlinked user, unknown/unconfigured provider)
// where retrying cannot help.
func isPermanentError(err error) bool {
	switch {
	case errors.Is(err, ErrNoSlackToken),
		errors.Is(err, ErrNoTelegramToken),
		errors.Is(err, ErrIdentityNotLinked),
		errors.Is(err, ErrUnknownProvider),
		errors.Is(err, ErrProviderNotConfigured),
		errors.Is(err, ErrMissingProvider):
		return true
	}
	return false
}

// finishStepWithRetry wraps FinishStepAndAdvance with retries for transient DB errors.
func (d *Dispatcher) finishStepWithRetry(stepID, leaseToken string, outcome model.JobStepStatus, result, stepError string) (model.AdvanceResult, error) {
	const maxRetries = 3
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		res, err := d.store.FinishStepAndAdvance(stepID, leaseToken, outcome, result, stepError)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if attempt < maxRetries {
			delay := time.Duration(attempt) * time.Second
			log.Printf("StepWorker: FinishStepAndAdvance failed (attempt %d/%d), retrying in %v: %v",
				attempt, maxRetries, delay, err)
			time.Sleep(delay)
		}
	}
	return 0, lastErr
}

func (d *Dispatcher) failStep(step *model.JobStep, msg string) {
	if step.LockedBy == nil {
		log.Printf("StepWorker: Step %s has no lease token, cannot finish", step.ID)
		return
	}
	res, err := d.finishStepWithRetry(step.ID, *step.LockedBy, model.JobStepStatusFailed, "", msg)
	if err != nil {
		log.Printf("StepWorker: FinishStepAndAdvance failed after retries for step %s: %v", step.ID, err)
		d.store.FailJob(step.JobID, fmt.Sprintf("finish step failed: %v", err))
		return
	}
	d.logAdvanceResult(step, res)
}

func (d *Dispatcher) completeStep(step *model.JobStep, result string) {
	if step.LockedBy == nil {
		log.Printf("StepWorker: Step %s has no lease token, cannot finish", step.ID)
		return
	}
	res, err := d.finishStepWithRetry(step.ID, *step.LockedBy, model.JobStepStatusSucceeded, result, "")
	if err != nil {
		log.Printf("StepWorker: FinishStepAndAdvance failed after retries for job %s: %v", step.JobID, err)
		d.store.FailJob(step.JobID, fmt.Sprintf("finish step failed: %v", err))
		return
	}
	d.logAdvanceResult(step, res)
}

func (d *Dispatcher) logAdvanceResult(step *model.JobStep, res model.AdvanceResult) {
	switch res {
	case model.AdvanceUnlockedNextStage:
		log.Printf("StepWorker: Step %s done, unlocked next stage", step.ID)
	case model.AdvanceJobFinished:
		log.Printf("StepWorker: Step %s done, job finished", step.ID)
	case model.AdvanceWaitingSiblings:
		log.Printf("StepWorker: Step %s done, waiting for siblings", step.ID)
	case model.AdvanceJobAlreadyTerminal:
		log.Printf("StepWorker: Step %s skipped, job already terminal", step.ID)
	case model.AdvanceAlreadyAdvanced:
		log.Printf("StepWorker: Step %s skipped, stage already advanced", step.ID)
	case model.AdvanceLeaseLost:
		log.Printf("StepWorker: Step %s lease lost", step.ID)
	}
}

// =================================================================================
// Ack Update Logic - Updates Slack messages when AG is acknowledged
// =================================================================================

func (d *Dispatcher) ackUpdateProcessingLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.ProcessAcknowledgedAlertGroups(ctx)
		}
	}
}

func (d *Dispatcher) ProcessAcknowledgedAlertGroups(ctx context.Context) {
	alertGroups, err := d.store.GetAcknowledgedAlertGroups()
	if err != nil {
		log.Printf("JobController: Error fetching acknowledged alert groups: %v", err)
		return
	}

	builder, err := builders.NewUpdateJobBuilder(d.cfg, d.store)
	if err != nil {
		log.Printf("JobController: Failed to init update job builder: %v", err)
		return
	}

	for _, ag := range alertGroups {
		// 1. Cancel any active escalation jobs - user acknowledged, no more escalation needed
		if ag.DedupKey != "" {
			if err := d.store.CancelJobByDedupKey(ag.DedupKey); err != nil {
				log.Printf("JobController: Failed to cancel escalation for acknowledged AG %s: %v", ag.ID, err)
			}
		}

		// 2. Create update job to update Slack message to yellow
		job, stages, steps, err := builder.Build(ag)
		if err != nil {
			if errors.Is(err, builders.ErrNoUpdatableDeliveries) {
				// No deliveries to update - mark as processed (permanent, expected)
				if markErr := d.store.MarkAckProcessed(ag.ID); markErr != nil {
					log.Printf("JobController: Failed to mark ack processed for %s: %v", ag.ID, markErr)
				}
				continue
			}
			// Other errors (e.g., ListDeliveries DB error) - transient, allow retry
			log.Printf("JobController: Build failed for %s (will retry): %v", ag.ID, err)
			continue
		}

		// Defensive nil check - Build contract guarantees job != nil when err == nil,
		// but guard against future changes to builder
		if job == nil {
			// Invariant violation - Build should never return (nil, _, nil)
			// Don't mark as processed - allow retry and investigation
			log.Printf("JobController: ERROR Build returned nil job for %s (will retry)", ag.ID)
			continue
		}

		// 3. Try to create the job
		if _, err := d.store.CreateJobWithDedup(job, stages, steps); err != nil {
			// Transient error - do NOT mark as processed, allow retry on next iteration
			log.Printf("JobController: Failed to create update job for %s (will retry): %v", ag.ID, err)
			continue
		}

		// 4. Job created successfully - mark as processed
		log.Printf("JobController: Update job created for acknowledged %s", ag.ID)
		if err := d.store.MarkAckProcessed(ag.ID); err != nil {
			log.Printf("JobController: Failed to mark ack processed for %s: %v", ag.ID, err)
		}
	}
}

// =================================================================================
// Resolution Logic (Legacy / Cleanup?)
// =================================================================================
// NOTE: Ideally Resolution should also be a Job, but preserving legacy loop for now.

func (d *Dispatcher) resolutionProcessingLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.ProcessResolvedAlertGroups(ctx)
		}
	}
}

func (d *Dispatcher) ProcessResolvedAlertGroups(ctx context.Context) {
	alertGroups, err := d.store.GetResolvedAlertGroups()
	if err != nil {
		log.Printf("JobController: Error fetching resolved alert groups: %v", err)
		return
	}

	builder := builders.NewResolutionJobBuilder(d.cfg, d.store)

	for _, ag := range alertGroups {
		// 1. Cancel any active escalation jobs
		if err := d.store.CancelJobByDedupKey(ag.DedupKey); err != nil {
			log.Printf("JobController: Failed to cancel jobs for resolved AG %s: %v", ag.ID, err)
		}

		// 2. Build resolution job
		job, stages, steps, err := builder.Build(ag)
		if err != nil {
			log.Printf("JobController: Build failed for resolved AG %s (will retry): %v", ag.ID, err)
			continue // transient error → retry next tick
		}

		if job != nil {
			// 3. Create resolution job
			if _, err := d.store.CreateJobWithDedup(job, stages, steps); err != nil {
				log.Printf("JobController: Failed to create resolution job for %s (will retry): %v", ag.ID, err)
				continue // transient error → retry next tick
			}
			log.Printf("JobController: Resolution job created for %s", ag.ID)
		}

		// 4. Mark as Closed — only after successful job creation (or no job needed)
		if err := d.store.UpdateAlertGroupStatus(ag.ID, model.AlertGroupStatusClosed); err != nil {
			log.Printf("JobController: Failed to close alert group %s: %v", ag.ID, err)
		}
	}
}

// =================================================================================
// Alert Update Logic — Updates Slack messages when alerts change in existing groups
// =================================================================================

func (d *Dispatcher) alertUpdateProcessingLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.ProcessAlertUpdates(ctx)
		}
	}
}

func (d *Dispatcher) ProcessAlertUpdates(ctx context.Context) {
	alertGroups, err := d.store.GetAlertGroupsPendingSlackUpdate()
	if err != nil {
		log.Printf("JobController: Error fetching alert groups pending Slack update: %v", err)
		return
	}

	if len(alertGroups) == 0 {
		return
	}

	builder, err := builders.NewUpdateJobBuilder(d.cfg, d.store)
	if err != nil {
		log.Printf("JobController: Failed to init update job builder for alert updates: %v", err)
		return
	}

	for _, ag := range alertGroups {
		job, stages, steps, err := builder.BuildWithDedup(ag, "update_alert")
		if errors.Is(err, builders.ErrNoUpdatableDeliveries) {
			// Distinguish: no deliveries at all (escalation pending) vs. deliveries exist but none updatable
			deliveries, dErr := d.store.ListDeliveries(ag.ID)
			if dErr != nil {
				log.Printf("JobController: Failed to list deliveries for %s (will retry): %v", ag.ID, dErr)
			} else if len(deliveries) > 0 {
				// Deliveries exist but none support updates — clear flag, no Slack to update
				log.Printf("JobController: No updatable deliveries for %s (%d total), clearing flag", ag.ID, len(deliveries))
				d.store.SetSlackUpdatePending(ag.ID, false)
			}
			// No deliveries at all — escalation still in progress, keep flag for retry
			continue
		}
		if err != nil {
			// Transient error (e.g., DB) — keep flag, retry next tick
			log.Printf("JobController: Build failed for alert update %s (will retry): %v", ag.ID, err)
			continue
		}

		if job == nil {
			log.Printf("JobController: ERROR Build returned nil job for alert update %s (will retry)", ag.ID)
			continue
		}

		// CreateJobWithDedup returns (existingID, nil) on dedup hit — not an error
		if _, err := d.store.CreateJobWithDedup(job, stages, steps); err != nil {
			// Transient error — keep flag, retry next tick
			log.Printf("JobController: Failed to create alert update job for %s (will retry): %v", ag.ID, err)
			continue
		}

		// Job created (or dedup hit) — clear flag
		log.Printf("JobController: Alert update job created for %s", ag.ID)
		if err := d.store.SetSlackUpdatePending(ag.ID, false); err != nil {
			log.Printf("JobController: Failed to clear slack update pending for %s: %v", ag.ID, err)
		}
	}
}

func strPtr(s string) *string {
	return &s
}
