package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"scholar-agent-backend/internal/models"
	"scholar-agent-backend/internal/store"
	"sort"
	"time"

	"github.com/google/uuid"
)

var ErrStaleExecution = errors.New("stale task execution")
var ErrPlanCanceled = errors.New("plan canceled")
var ErrPlanStateChanged = errors.New("plan state changed")

// EventPublisher emits incremental execution events.
type EventPublisher interface {
	Publish(planID string, event models.PlanEvent)
}

// NoopEventPublisher is used until the SSE/event bus layer is wired in.
type NoopEventPublisher struct{}

func (p *NoopEventPublisher) Publish(planID string, event models.PlanEvent) {
	_ = planID
	_ = event
}

// Scheduler executes one plan graph by continuously promoting and running ready nodes.
type Scheduler struct {
	store         store.PlanStore
	executor      TaskExecutor
	publisher     EventPublisher
	maxConcurrent int
	onTerminal    func(context.Context, *models.PlanGraph)
}

func NewScheduler(planStore store.PlanStore, executor TaskExecutor, publisher EventPublisher, maxConcurrent int) *Scheduler {
	if executor == nil {
		executor = NewDefaultTaskExecutor()
	}
	if publisher == nil {
		publisher = &NoopEventPublisher{}
	}
	if maxConcurrent <= 0 {
		maxConcurrent = 4
	}

	return &Scheduler{
		store:         planStore,
		executor:      executor,
		publisher:     publisher,
		maxConcurrent: maxConcurrent,
	}
}

func (s *Scheduler) SetOnTerminal(fn func(context.Context, *models.PlanGraph)) {
	s.onTerminal = fn
}

func (s *Scheduler) CancelPlan(planID, reason string) error {
	now := time.Now()
	if reason == "" {
		reason = "canceled by user"
	}
	if err := s.updateAndPublish(planID, func(plan *models.PlanGraph) error {
		if plan.Status == models.StatusCompleted || plan.Status == models.StatusFailed || plan.Status == models.StatusCanceled {
			return fmt.Errorf("plan cannot be canceled from status %s", plan.Status)
		}
		plan.Status = models.StatusCanceled
		plan.Usage.FinishedAt = &now
		plan.UpdatedAt = now
		for _, task := range plan.Nodes {
			if task == nil {
				continue
			}
			switch task.Status {
			case models.StatusPending, models.StatusReady, models.StatusInProgress, models.StatusBlocked:
				task.Status = models.StatusCanceled
				task.Error = reason
				task.ExecutionID = ""
				task.LeaseOwner = ""
				task.LeaseExpiresAt = nil
				task.FinishedAt = &now
				task.UpdatedAt = now
			}
		}
		fillMeta(plan)
		return nil
	}, func() []models.PlanEvent {
		return []models.PlanEvent{{PlanID: planID, EventType: "plan_canceled", TaskStatus: string(models.StatusCanceled), Payload: map[string]any{"reason": reason}, Timestamp: now}}
	}); err != nil {
		return err
	}
	return nil
}

func (s *Scheduler) RetryTask(planID, taskID string) error {
	now := time.Now()
	if err := s.updateAndPublish(planID, func(plan *models.PlanGraph) error {
		task := getNodeByID(plan, taskID)
		if task == nil {
			return fmt.Errorf("task not found: %s", taskID)
		}
		if task.Status != models.StatusFailed && task.Status != models.StatusBlocked && task.Status != models.StatusCanceled {
			return fmt.Errorf("task cannot be retried from status %s", task.Status)
		}
		resetTaskForRetry(task, now)
		resetBlockedDependents(plan, taskID, now)
		plan.Status = models.StatusPending
		plan.Usage.FinishedAt = nil
		plan.UpdatedAt = now
		fillMeta(plan)
		return nil
	}, func() []models.PlanEvent {
		return []models.PlanEvent{{PlanID: planID, EventType: "task_retry_requested", TaskID: taskID, TaskStatus: string(models.StatusPending), Timestamp: now}}
	}); err != nil {
		return err
	}
	return nil
}

func (s *Scheduler) ReassignTask(planID, taskID, assignedTo string) error {
	now := time.Now()
	if assignedTo == "" {
		return fmt.Errorf("assigned agent is required")
	}
	if err := s.updateAndPublish(planID, func(plan *models.PlanGraph) error {
		task := getNodeByID(plan, taskID)
		if task == nil {
			return fmt.Errorf("task not found: %s", taskID)
		}
		if task.Status == models.StatusCompleted || task.Status == models.StatusSkipped {
			return fmt.Errorf("completed task cannot be reassigned")
		}
		task.AssignedTo = assignedTo
		resetTaskForRetry(task, now)
		resetBlockedDependents(plan, taskID, now)
		plan.Status = models.StatusPending
		plan.Usage.FinishedAt = nil
		plan.UpdatedAt = now
		fillMeta(plan)
		return nil
	}, func() []models.PlanEvent {
		return []models.PlanEvent{{PlanID: planID, EventType: "task_reassigned", TaskID: taskID, TaskStatus: string(models.StatusPending), Payload: map[string]any{"assigned_to": assignedTo}, Timestamp: now}}
	}); err != nil {
		return err
	}
	return nil
}

func (s *Scheduler) ExecutePlan(ctx context.Context, planID string) error {
	if s.store == nil {
		return fmt.Errorf("plan store is nil")
	}

	if err := s.updateAndPublish(planID, func(plan *models.PlanGraph) error {
		if plan.Status == models.StatusInProgress {
			return fmt.Errorf("plan is already running")
		}
		if plan.Approval.Required && plan.Approval.Status != "approved" {
			return fmt.Errorf("plan requires approval")
		}
		if plan.Status != models.StatusPending && plan.Status != models.StatusReady {
			return fmt.Errorf("plan cannot run from status %s", plan.Status)
		}
		now := time.Now()
		plan.Status = models.StatusInProgress
		plan.Usage.StartedAt = &now
		plan.Usage.FinishedAt = nil
		plan.UpdatedAt = now
		return nil
	}, func() []models.PlanEvent {
		return []models.PlanEvent{{PlanID: planID, EventType: "plan_started", Timestamp: time.Now()}}
	}); err != nil {
		return err
	}

executionLoop:
	for {
		select {
		case <-ctx.Done():
			// The run context also carries the API-level duration budget.
			// Invalidate leases and publish a terminal state before abandoning it.
			if err := s.CancelPlan(planID, ctx.Err().Error()); err != nil {
				log.Printf("scheduler: finalize canceled context for plan %s: %v", planID, err)
			}
			return ctx.Err()
		default:
		}

		plan, err := s.store.GetPlan(planID)
		if err != nil {
			return err
		}
		if plan.Status == models.StatusCanceled {
			return nil
		}
		if budgetExceeded(plan) {
			_ = s.CancelPlan(planID, "run budget exceeded")
			return fmt.Errorf("run budget exceeded")
		}

		readyEvents := []models.PlanEvent{}
		if err := s.updateAndPublish(planID, func(current *models.PlanGraph) error {
			if current.Status == models.StatusCanceled {
				return ErrPlanCanceled
			}
			readyEvents = s.promotePendingNodesToReady(current)
			fillMeta(current)
			return nil
		}, func() []models.PlanEvent { return readyEvents }); err != nil {
			if errors.Is(err, ErrPlanCanceled) {
				return nil
			}
			return err
		}
		plan, err = s.store.GetPlan(planID)
		if err != nil {
			return err
		}

		availableSlots := s.maxConcurrent - countInProgress(plan)
		if plan.Budget.MaxTaskAttempts > 0 {
			remainingAttempts := plan.Budget.MaxTaskAttempts - plan.Usage.TaskAttempts
			if availableSlots > remainingAttempts {
				availableSlots = remainingAttempts
			}
		}
		ready := s.findReadyNodes(plan, availableSlots)
		if len(ready) == 0 {
			if allTerminal(plan) {
				finalStatus := models.StatusCompleted
				if err := s.updateAndPublish(planID, func(current *models.PlanGraph) error {
					if current.Status == models.StatusCanceled {
						return ErrPlanCanceled
					}
					if !allTerminal(current) {
						return ErrPlanStateChanged
					}
					if hasFailure(current) {
						finalStatus = models.StatusFailed
					}
					now := time.Now()
					current.Status = finalStatus
					current.Usage.FinishedAt = &now
					current.UpdatedAt = now
					fillMeta(current)
					return nil
				}, func() []models.PlanEvent {
					eventType := "plan_completed"
					if finalStatus == models.StatusFailed {
						eventType = "plan_failed"
					}
					return []models.PlanEvent{{PlanID: planID, EventType: eventType, Timestamp: time.Now()}}
				}); err != nil {
					if errors.Is(err, ErrPlanCanceled) {
						return nil
					}
					if errors.Is(err, ErrPlanStateChanged) {
						continue executionLoop
					}
					return err
				}

				if s.onTerminal != nil {
					if latest, getErr := s.store.GetPlan(planID); getErr == nil && latest != nil {
						s.onTerminal(ctx, latest)
					} else {
						s.onTerminal(ctx, plan)
					}
				}
				return nil
			}

			if noRunning(plan) {
				if err := s.updateAndPublish(planID, func(current *models.PlanGraph) error {
					if current.Status == models.StatusCanceled {
						return ErrPlanCanceled
					}
					if !noRunning(current) || len(s.promotePendingNodesToReady(current)) > 0 {
						return ErrPlanStateChanged
					}
					current.Status = models.StatusFailed
					current.UpdatedAt = time.Now()
					fillMeta(current)
					return nil
				}, func() []models.PlanEvent {
					return []models.PlanEvent{{PlanID: planID, EventType: "plan_failed", Payload: map[string]any{"reason": "deadlock_or_unsatisfied_dependencies"}, Timestamp: time.Now()}}
				}); err != nil {
					if errors.Is(err, ErrPlanCanceled) {
						return nil
					}
					if errors.Is(err, ErrPlanStateChanged) {
						continue executionLoop
					}
					return err
				}
				if s.onTerminal != nil {
					if latest, getErr := s.store.GetPlan(planID); getErr == nil && latest != nil {
						s.onTerminal(ctx, latest)
					} else {
						s.onTerminal(ctx, plan)
					}
				}
				return nil
			}

			time.Sleep(200 * time.Millisecond)
			continue
		}

		if err := s.runReadyNodes(ctx, planID, ready); err != nil {
			if errors.Is(err, ErrPlanCanceled) {
				return nil
			}
			return err
		}

		time.Sleep(150 * time.Millisecond)
	}
}

func (s *Scheduler) promotePendingNodesToReady(plan *models.PlanGraph) []models.PlanEvent {
	events := []models.PlanEvent{}
	for _, node := range plan.Nodes {
		if node == nil || node.Status != models.StatusPending {
			continue
		}
		if allDependenciesCompleted(plan, node) && artifactsSatisfied(plan, node) {
			node.Status = models.StatusReady
			node.UpdatedAt = time.Now()
			events = append(events, models.PlanEvent{
				PlanID:     plan.ID,
				EventType:  "task_ready",
				TaskID:     node.ID,
				TaskStatus: string(node.Status),
				Payload: map[string]any{
					"name":   node.Name,
					"inputs": summarizeTaskInputs(plan, node),
				},
				Timestamp: time.Now(),
			})
		}
	}
	if len(events) > 0 {
		plan.UpdatedAt = time.Now()
	}
	return events
}

func (s *Scheduler) findReadyNodes(plan *models.PlanGraph, availableSlots int) []*models.TaskNode {
	if availableSlots <= 0 {
		return nil
	}

	ready := []*models.TaskNode{}
	for _, node := range plan.Nodes {
		if node != nil && node.Status == models.StatusReady {
			ready = append(ready, node)
		}
	}

	if len(ready) == 0 {
		return nil
	}

	sort.SliceStable(ready, func(i, j int) bool {
		if ready[i].Priority != ready[j].Priority {
			return ready[i].Priority > ready[j].Priority
		}
		return ready[i].CreatedAt.Before(ready[j].CreatedAt)
	})

	serial := make([]*models.TaskNode, 0, len(ready))
	parallel := make([]*models.TaskNode, 0, len(ready))
	for _, node := range ready {
		if node.Parallelizable {
			parallel = append(parallel, node)
		} else {
			serial = append(serial, node)
		}
	}

	if len(serial) > 0 {
		return serial[:1]
	}
	if len(parallel) > availableSlots {
		return parallel[:availableSlots]
	}
	return parallel
}

func (s *Scheduler) runReadyNodes(ctx context.Context, planID string, ready []*models.TaskNode) error {
	for _, task := range ready {
		executionID, leaseOwner, err := s.markTaskStarted(planID, task.ID)
		if err != nil {
			return err
		}

		taskID := task.ID
		go func() {
			plan, err := s.store.GetPlan(planID)
			if err != nil {
				log.Printf("scheduler: failed to load plan %s for task %s: %v", planID, taskID, err)
				return
			}
			currentTask := getNodeByID(plan, taskID)
			if currentTask == nil {
				log.Printf("scheduler: task not found during execution: %s", taskID)
				return
			}

			logChannel := make(chan string, 64)
			execCtx := context.WithValue(ctx, "logChannel", logChannel)
			taskTimeout := time.Duration(currentTask.TimeoutSeconds) * time.Second
			if taskTimeout <= 0 {
				taskTimeout = 10 * time.Minute
			}
			taskCtx, cancelTask := context.WithTimeout(execCtx, taskTimeout)
			forwardDone := make(chan struct{})
			go s.forwardTaskLogs(planID, taskID, logChannel, forwardDone)

			result, execErr := s.executor.ExecuteTask(taskCtx, plan, currentTask)
			if errors.Is(taskCtx.Err(), context.DeadlineExceeded) {
				execErr = fmt.Errorf("task execution timed out after %s: %w", taskTimeout, taskCtx.Err())
			} else if errors.Is(taskCtx.Err(), context.Canceled) {
				execErr = context.Canceled
			}
			cancelTask()
			close(logChannel)
			<-forwardDone

			if execErr != nil {
				result = &models.TaskExecutionResult{
					Status: models.StatusFailed,
					Error:  execErr.Error(),
				}
			}
			if result == nil {
				result = &models.TaskExecutionResult{
					Status: models.StatusFailed,
					Error:  "task execution returned nil result",
				}
			}

			if result.Status == models.StatusFailed {
				retrying, err := s.markTaskFailed(planID, taskID, executionID, leaseOwner, result)
				if errors.Is(err, ErrStaleExecution) {
					s.publishDiscardedResult(planID, taskID, executionID, leaseOwner)
					return
				}
				if err != nil {
					log.Printf("scheduler: markTaskFailed failed for %s: %v", taskID, err)
					return
				}
				if !retrying && shouldBlockOnFailure(currentTask) {
					if err := s.blockDependents(planID, taskID, currentTask.ExecutionEpoch); err != nil {
						log.Printf("scheduler: blockDependents failed for %s: %v", taskID, err)
						return
					}
				}
				return
			}

			if err := s.markTaskCompleted(planID, taskID, executionID, leaseOwner, result); errors.Is(err, ErrStaleExecution) {
				s.publishDiscardedResult(planID, taskID, executionID, leaseOwner)
				return
			} else if err != nil {
				log.Printf("scheduler: markTaskCompleted failed for %s: %v", taskID, err)
				return
			}
		}()
	}
	return nil
}

func (s *Scheduler) forwardTaskLogs(planID, taskID string, logChannel <-chan string, done chan<- struct{}) {
	defer close(done)
	for line := range logChannel {
		if err := s.publishAndStore(planID, models.PlanEvent{
			PlanID:     planID,
			EventType:  "task_log",
			TaskID:     taskID,
			TaskStatus: string(models.StatusInProgress),
			Payload: map[string]any{
				"message": line,
			},
			Timestamp: time.Now(),
		}); err != nil {
			log.Printf("scheduler: persist task log for %s: %v", taskID, err)
		}
	}
}

func (s *Scheduler) markTaskStarted(planID, taskID string) (string, string, error) {
	now := time.Now()
	executionID := uuid.NewString()
	leaseOwner := ""
	if err := s.updateAndPublish(planID, func(plan *models.PlanGraph) error {
		if plan.Status == models.StatusCanceled {
			return ErrPlanCanceled
		}
		task := getNodeByID(plan, taskID)
		if task == nil {
			return fmt.Errorf("task not found: %s", taskID)
		}
		if plan.Budget.MaxTaskAttempts > 0 && plan.Usage.TaskAttempts >= plan.Budget.MaxTaskAttempts {
			return fmt.Errorf("task attempt budget exceeded")
		}
		leaseOwner = task.AssignedTo
		timeout := time.Duration(task.TimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = 10 * time.Minute
		}
		leaseExpiresAt := now.Add(timeout)
		task.Status = models.StatusInProgress
		task.ExecutionID = executionID
		task.ExecutionEpoch++
		task.LeaseOwner = leaseOwner
		task.LeaseExpiresAt = &leaseExpiresAt
		task.RunCount++
		task.StartedAt = &now
		task.FinishedAt = nil
		task.UpdatedAt = now
		plan.Usage.TaskAttempts++
		fillMeta(plan)
		return nil
	}, func() []models.PlanEvent {
		return []models.PlanEvent{{
			PlanID:      planID,
			EventType:   "task_started",
			TaskID:      taskID,
			TaskStatus:  string(models.StatusInProgress),
			ExecutionID: executionID,
			Payload:     map[string]any{"lease_owner": leaseOwner},
			Timestamp:   now,
		}}
	}); err != nil {
		return "", "", err
	}

	return executionID, leaseOwner, nil
}

func (s *Scheduler) markTaskCompleted(planID, taskID, executionID, leaseOwner string, result *models.TaskExecutionResult) error {
	now := time.Now()
	if err := s.updateAndPublish(planID, func(plan *models.PlanGraph) error {
		task := getNodeByID(plan, taskID)
		if task == nil {
			return fmt.Errorf("task not found: %s", taskID)
		}
		if !executionMatches(task, executionID, leaseOwner) {
			return ErrStaleExecution
		}
		task.Status = models.StatusCompleted
		task.Result = result.Result
		task.Code = result.Code
		task.StructuredData = result.StructuredData
		task.ImageBase64 = result.ImageBase64
		task.Error = ""
		task.ExecutionID = ""
		task.LeaseOwner = ""
		task.LeaseExpiresAt = nil
		task.FinishedAt = &now
		task.UpdatedAt = now

		for _, artifact := range result.Artifacts {
			plan.Artifacts[artifact.Key] = artifact
		}

		fillMeta(plan)
		return nil
	}, func() []models.PlanEvent {
		events := []models.PlanEvent{}
		if len(result.Artifacts) > 0 {
			keys := make([]string, 0, len(result.Artifacts))
			for _, artifact := range result.Artifacts {
				keys = append(keys, artifact.Key)
			}
			events = append(events, models.PlanEvent{
				PlanID:    planID,
				EventType: "artifact_created",
				TaskID:    taskID,
				Payload: map[string]any{
					"artifact_keys": keys,
					"artifacts":     summarizeArtifacts(result.Artifacts),
				},
				Timestamp: now,
			})
		}

		events = append(events, models.PlanEvent{
			PlanID:      planID,
			EventType:   "task_completed",
			TaskID:      taskID,
			TaskStatus:  string(models.StatusCompleted),
			ExecutionID: executionID,
			Payload: map[string]any{
				"result_summary":  result.Result,
				"result":          result.Result,
				"code":            result.Code,
				"structured_data": result.StructuredData,
				"image_base64":    result.ImageBase64,
			},
			Timestamp: now,
		})
		return events
	}); err != nil {
		return err
	}

	return nil
}

func (s *Scheduler) markTaskFailed(planID, taskID, executionID, leaseOwner string, result *models.TaskExecutionResult) (bool, error) {
	now := time.Now()
	retrying := false
	if err := s.updateAndPublish(planID, func(plan *models.PlanGraph) error {
		task := getNodeByID(plan, taskID)
		if task == nil {
			return fmt.Errorf("task not found: %s", taskID)
		}
		if !executionMatches(task, executionID, leaseOwner) {
			return ErrStaleExecution
		}
		task.Error = result.Error
		task.ExecutionID = ""
		task.LeaseOwner = ""
		task.LeaseExpiresAt = nil
		if task.RunCount <= task.RetryLimit {
			task.Status = models.StatusPending
			task.FinishedAt = nil
			retrying = true
		} else {
			task.Status = models.StatusFailed
			task.FinishedAt = &now
		}
		task.UpdatedAt = now
		fillMeta(plan)
		return nil
	}, func() []models.PlanEvent {
		events := []models.PlanEvent{}
		if retrying {
			events = append(events, models.PlanEvent{PlanID: planID, EventType: "task_retrying", TaskID: taskID, TaskStatus: string(models.StatusPending), ExecutionID: executionID, Payload: map[string]any{"error": result.Error}, Timestamp: now})
			return events
		}
		events = append(events, models.PlanEvent{
			PlanID:      planID,
			EventType:   "task_failed",
			TaskID:      taskID,
			TaskStatus:  string(models.StatusFailed),
			ExecutionID: executionID,
			Payload: map[string]any{
				"error": result.Error,
			},
			Timestamp: now,
		})
		return events
	}); err != nil {
		return false, err
	}

	return retrying, nil
}

func summarizeTaskInputs(plan *models.PlanGraph, node *models.TaskNode) []map[string]any {
	if plan == nil || node == nil {
		return nil
	}

	inputs := []map[string]any{}
	for _, key := range node.RequiredArtifacts {
		artifact, ok := plan.Artifacts[key]
		if !ok {
			continue
		}
		inputs = append(inputs, map[string]any{
			"key":              key,
			"type":             artifact.Type,
			"producer_task_id": artifact.ProducerTaskID,
			"value_preview":    previewValue(artifact.Value),
		})
	}
	return inputs
}

func summarizeArtifacts(artifacts []models.Artifact) []map[string]any {
	out := make([]map[string]any, 0, len(artifacts))
	for _, artifact := range artifacts {
		out = append(out, map[string]any{
			"key":              artifact.Key,
			"type":             artifact.Type,
			"producer_task_id": artifact.ProducerTaskID,
			"value_preview":    previewValue(artifact.Value),
		})
	}
	return out
}

func previewValue(value string) string {
	const limit = 1000
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "...[truncated]"
}

func executionMatches(task *models.TaskNode, executionID, leaseOwner string) bool {
	return task != nil &&
		task.Status == models.StatusInProgress &&
		task.ExecutionID == executionID &&
		task.LeaseOwner == leaseOwner &&
		task.AssignedTo == leaseOwner
}

func resetTaskForRetry(task *models.TaskNode, now time.Time) {
	task.Status = models.StatusPending
	task.Result = ""
	task.Error = ""
	task.ExecutionID = ""
	task.LeaseOwner = ""
	task.LeaseExpiresAt = nil
	task.StartedAt = nil
	task.FinishedAt = nil
	task.UpdatedAt = now
}

func resetBlockedDependents(plan *models.PlanGraph, taskID string, now time.Time) {
	visited := map[string]bool{}
	var visit func(string)
	visit = func(currentID string) {
		for _, next := range getOutgoingNodes(plan, currentID) {
			if next == nil || visited[next.ID] {
				continue
			}
			visited[next.ID] = true
			if next.Status == models.StatusBlocked || next.Status == models.StatusCanceled {
				resetTaskForRetry(next, now)
			}
			visit(next.ID)
		}
	}
	visit(taskID)
}

func budgetExceeded(plan *models.PlanGraph) bool {
	if plan == nil {
		return false
	}
	if plan.Budget.MaxTaskAttempts > 0 && plan.Usage.TaskAttempts >= plan.Budget.MaxTaskAttempts && noRunning(plan) && !allTerminal(plan) {
		return true
	}
	return plan.Budget.MaxDurationSec > 0 && plan.Usage.StartedAt != nil && time.Since(*plan.Usage.StartedAt) > time.Duration(plan.Budget.MaxDurationSec)*time.Second
}

func (s *Scheduler) publishDiscardedResult(planID, taskID, executionID, leaseOwner string) {
	if err := s.publishAndStore(planID, models.PlanEvent{
		PlanID:      planID,
		EventType:   "task_result_discarded",
		TaskID:      taskID,
		ExecutionID: executionID,
		Payload: map[string]any{
			"reason":      "stale_execution_lease",
			"lease_owner": leaseOwner,
		},
		Timestamp: time.Now(),
	}); err != nil {
		log.Printf("scheduler: persist discarded result for %s: %v", taskID, err)
	}
}

func (s *Scheduler) blockDependents(planID, taskID string, executionEpoch int) error {
	events := []models.PlanEvent{}
	err := s.updateAndPublish(planID, func(plan *models.PlanGraph) error {
		failedTask := getNodeByID(plan, taskID)
		// A manual retry or reassignment may have won after task_failed was
		// published. Only the still-current failed attempt may block children.
		if failedTask == nil || failedTask.Status != models.StatusFailed || failedTask.ExecutionEpoch != executionEpoch {
			return nil
		}
		visited := map[string]bool{}
		var visit func(string)
		visit = func(currentID string) {
			for _, next := range getOutgoingNodes(plan, currentID) {
				if next == nil || visited[next.ID] {
					continue
				}
				visited[next.ID] = true
				if next.Status == models.StatusPending || next.Status == models.StatusReady {
					next.Status = models.StatusBlocked
					next.UpdatedAt = time.Now()
					events = append(events, models.PlanEvent{
						PlanID:     planID,
						EventType:  "task_blocked",
						TaskID:     next.ID,
						TaskStatus: string(models.StatusBlocked),
						Payload: map[string]any{
							"reason":           "upstream_failed",
							"upstream_task_id": currentID,
						},
						Timestamp: time.Now(),
					})
				}
				visit(next.ID)
			}
		}
		visit(taskID)
		fillMeta(plan)
		return nil
	}, func() []models.PlanEvent { return events })
	if err != nil {
		return err
	}
	return nil
}

// updateAndPublish commits a state transition and its entire event batch before
// notifying live subscribers. Event builders run under the store lock and must
// not read from the store. Publication is best effort; history is authoritative.
func (s *Scheduler) updateAndPublish(planID string, update func(*models.PlanGraph) error, buildEvents func() []models.PlanEvent) error {
	var committed []models.PlanEvent
	if err := s.store.UpdatePlanWithEvents(planID, func(plan *models.PlanGraph) ([]models.PlanEvent, error) {
		if err := update(plan); err != nil {
			return nil, err
		}
		committed = buildEvents()
		for i := range committed {
			prepareEvent(planID, plan.TraceID, &committed[i])
		}
		return committed, nil
	}); err != nil {
		return fmt.Errorf("commit plan transition and events: %w", err)
	}
	for _, event := range committed {
		s.publisher.Publish(planID, event)
	}
	return nil
}

func prepareEvent(planID, traceID string, event *models.PlanEvent) {
	event.PlanID = planID
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	if event.TraceID == "" {
		event.TraceID = traceID
	}
	if event.SpanID == "" && event.TaskID != "" {
		event.SpanID = "task_" + event.TaskID
	}
}

func (s *Scheduler) publishAndStore(planID string, event models.PlanEvent) error {
	plan, err := s.store.GetPlan(planID)
	if err != nil {
		return fmt.Errorf("load plan for event: %w", err)
	}
	prepareEvent(planID, plan.TraceID, &event)
	if err := s.store.AppendEvent(planID, event); err != nil {
		return fmt.Errorf("persist %s event: %w", event.EventType, err)
	}
	s.publisher.Publish(planID, event)
	return nil
}

func allTerminal(plan *models.PlanGraph) bool {
	for _, node := range plan.Nodes {
		if node == nil {
			continue
		}
		switch node.Status {
		case models.StatusPending, models.StatusReady, models.StatusInProgress:
			return false
		}
	}
	return true
}

func hasFailure(plan *models.PlanGraph) bool {
	for _, node := range plan.Nodes {
		if node != nil && (node.Status == models.StatusFailed || node.Status == models.StatusBlocked || node.Status == models.StatusCanceled) {
			return true
		}
	}
	return false
}

func noRunning(plan *models.PlanGraph) bool {
	for _, node := range plan.Nodes {
		if node != nil && node.Status == models.StatusInProgress {
			return false
		}
	}
	return true
}

func countInProgress(plan *models.PlanGraph) int {
	count := 0
	for _, node := range plan.Nodes {
		if node != nil && node.Status == models.StatusInProgress {
			count++
		}
	}
	return count
}

func fillMeta(plan *models.PlanGraph) {
	meta := models.GraphMeta{
		TotalNodes: len(plan.Nodes),
	}
	for _, node := range plan.Nodes {
		if node == nil {
			continue
		}
		switch node.Status {
		case models.StatusCompleted:
			meta.CompletedNodes++
		case models.StatusFailed:
			meta.FailedNodes++
		case models.StatusBlocked:
			meta.BlockedNodes++
		case models.StatusInProgress:
			meta.InProgressNodes++
		case models.StatusReady:
			meta.ReadyNodes++
		}
	}
	plan.Meta = meta
}
