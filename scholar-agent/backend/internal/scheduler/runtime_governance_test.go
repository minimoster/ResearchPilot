package scheduler

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"scholar-agent-backend/internal/models"
	"scholar-agent-backend/internal/store"
)

type taskExecutorFunc func(context.Context, *models.PlanGraph, *models.TaskNode) (*models.TaskExecutionResult, error)

func (fn taskExecutorFunc) ExecuteTask(ctx context.Context, plan *models.PlanGraph, task *models.TaskNode) (*models.TaskExecutionResult, error) {
	return fn(ctx, plan, task)
}

func TestSchedulerRetriesFailedAttemptWithinLimit(t *testing.T) {
	planStore := store.NewMemoryPlanStore()
	plan := governanceTestPlan("retry-plan", 1)
	plan.Nodes[0].RetryLimit = 1
	if err := planStore.SavePlan(plan); err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int32
	runner := NewScheduler(planStore, taskExecutorFunc(func(context.Context, *models.PlanGraph, *models.TaskNode) (*models.TaskExecutionResult, error) {
		if attempts.Add(1) == 1 {
			return &models.TaskExecutionResult{Status: models.StatusFailed, Error: "transient"}, nil
		}
		return &models.TaskExecutionResult{Status: models.StatusCompleted, Result: "recovered"}, nil
	}), &NoopEventPublisher{}, 1)

	if err := runner.ExecutePlan(context.Background(), plan.ID); err != nil {
		t.Fatal(err)
	}
	finalPlan, _ := planStore.GetPlan(plan.ID)
	if finalPlan.Status != models.StatusCompleted || finalPlan.Nodes[0].RunCount != 2 || finalPlan.Nodes[0].Result != "recovered" {
		t.Fatalf("automatic retry did not recover plan: %#v", finalPlan.Nodes[0])
	}
}

func TestSchedulerEnforcesTaskTimeout(t *testing.T) {
	planStore := store.NewMemoryPlanStore()
	plan := governanceTestPlan("timeout-plan", 1)
	plan.Nodes[0].TimeoutSeconds = 1
	if err := planStore.SavePlan(plan); err != nil {
		t.Fatal(err)
	}
	runner := NewScheduler(planStore, taskExecutorFunc(func(ctx context.Context, _ *models.PlanGraph, _ *models.TaskNode) (*models.TaskExecutionResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}), &NoopEventPublisher{}, 1)

	if err := runner.ExecutePlan(context.Background(), plan.ID); err != nil {
		t.Fatal(err)
	}
	finalPlan, _ := planStore.GetPlan(plan.ID)
	if finalPlan.Status != models.StatusFailed {
		t.Fatalf("timeout did not fail plan: status=%s task=%#v", finalPlan.Status, finalPlan.Nodes[0])
	}
	if !strings.Contains(finalPlan.Nodes[0].Error, "timed out") {
		t.Fatalf("timeout failure should record a useful error: %q", finalPlan.Nodes[0].Error)
	}
}

func TestSchedulerCancelsPlanWhenAttemptBudgetIsExhausted(t *testing.T) {
	planStore := store.NewMemoryPlanStore()
	plan := governanceTestPlan("budget-plan", 2)
	plan.Nodes[1].Dependencies = []string{plan.Nodes[0].ID}
	plan.Budget.MaxTaskAttempts = 1
	if err := planStore.SavePlan(plan); err != nil {
		t.Fatal(err)
	}
	runner := NewScheduler(planStore, NewDefaultTaskExecutor(), &NoopEventPublisher{}, 1)

	if err := runner.ExecutePlan(context.Background(), plan.ID); err == nil {
		t.Fatal("expected execution to report exhausted budget")
	}
	finalPlan, _ := planStore.GetPlan(plan.ID)
	if finalPlan.Status != models.StatusCanceled || finalPlan.Usage.TaskAttempts != 1 {
		t.Fatalf("budget did not cancel plan: status=%s usage=%#v", finalPlan.Status, finalPlan.Usage)
	}
}

func TestCancelPlanRejectsTerminalPlan(t *testing.T) {
	planStore := store.NewMemoryPlanStore()
	plan := governanceTestPlan("terminal-plan", 1)
	plan.Status = models.StatusCompleted
	plan.Nodes[0].Status = models.StatusCompleted
	if err := planStore.SavePlan(plan); err != nil {
		t.Fatal(err)
	}
	if err := NewScheduler(planStore, nil, nil, 1).CancelPlan(plan.ID, "too late"); err == nil {
		t.Fatal("expected terminal plan cancellation to be rejected")
	}
}

func TestCanceledPlanCannotBeResurrectedByLateTaskResult(t *testing.T) {
	planStore := store.NewMemoryPlanStore()
	plan := governanceTestPlan("cancel-race-plan", 1)
	if err := planStore.SavePlan(plan); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	runner := NewScheduler(planStore, taskExecutorFunc(func(context.Context, *models.PlanGraph, *models.TaskNode) (*models.TaskExecutionResult, error) {
		close(started)
		<-release
		return &models.TaskExecutionResult{Status: models.StatusCompleted, Result: "late result"}, nil
	}), &NoopEventPublisher{}, 1)
	done := make(chan error, 1)
	go func() { done <- runner.ExecutePlan(context.Background(), plan.ID) }()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("task did not start")
	}
	if err := runner.CancelPlan(plan.ID, "test cancellation"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler did not observe cancellation")
	}
	close(release)
	time.Sleep(100 * time.Millisecond)
	finalPlan, _ := planStore.GetPlan(plan.ID)
	if finalPlan.Status != models.StatusCanceled || finalPlan.Nodes[0].Status != models.StatusCanceled || finalPlan.Nodes[0].Result != "" {
		t.Fatalf("late task result resurrected canceled state: %#v", finalPlan.Nodes[0])
	}
}

func governanceTestPlan(id string, nodeCount int) *models.PlanGraph {
	now := time.Now()
	plan := &models.PlanGraph{
		ID:        id,
		TraceID:   "trace-" + id,
		Status:    models.StatusPending,
		Budget:    models.RunBudget{MaxTaskAttempts: 10, MaxDurationSec: 30},
		Artifacts: map[string]models.Artifact{},
		CreatedAt: now,
		UpdatedAt: now,
	}
	for i := 0; i < nodeCount; i++ {
		plan.Nodes = append(plan.Nodes, &models.TaskNode{
			ID:             fmt.Sprintf("%s-task-%d", id, i+1),
			Name:           "Test task",
			Type:           "execute_code",
			AssignedTo:     "sandbox_agent",
			Status:         models.StatusPending,
			TimeoutSeconds: 5,
			CreatedAt:      now,
			UpdatedAt:      now,
		})
	}
	return plan
}

func TestSchedulerDeadlineTerminatesPlan(t *testing.T) {
	planStore := store.NewMemoryPlanStore()
	plan := governanceTestPlan("deadline-plan", 1)
	if err := planStore.SavePlan(plan); err != nil {
		t.Fatal(err)
	}
	runner := NewScheduler(planStore, taskExecutorFunc(func(ctx context.Context, _ *models.PlanGraph, _ *models.TaskNode) (*models.TaskExecutionResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}), nil, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := runner.ExecutePlan(ctx, plan.ID); err != context.DeadlineExceeded {
		t.Fatalf("expected deadline error, got %v", err)
	}
	finalPlan, _ := planStore.GetPlan(plan.ID)
	if finalPlan.Status != models.StatusCanceled || finalPlan.Usage.FinishedAt == nil {
		t.Fatalf("deadline left plan active: status=%s usage=%#v", finalPlan.Status, finalPlan.Usage)
	}
	if finalPlan.Nodes[0].Status == models.StatusInProgress || finalPlan.Nodes[0].ExecutionID != "" {
		t.Fatalf("deadline left a task lease active: %#v", finalPlan.Nodes[0])
	}
	events, _ := planStore.ListEvents(plan.ID)
	foundTerminal := false
	for _, event := range events {
		foundTerminal = foundTerminal || event.EventType == "plan_canceled"
	}
	if !foundTerminal {
		t.Fatal("deadline did not publish a terminal event")
	}
}

func TestSchedulerParallelBatchRespectsRemainingAttemptBudget(t *testing.T) {
	planStore := store.NewMemoryPlanStore()
	plan := governanceTestPlan("parallel-budget-plan", 2)
	plan.Budget.MaxTaskAttempts = 1
	for _, node := range plan.Nodes {
		node.Parallelizable = true
	}
	if err := planStore.SavePlan(plan); err != nil {
		t.Fatal(err)
	}
	runner := NewScheduler(planStore, NewDefaultTaskExecutor(), nil, 2)
	if err := runner.ExecutePlan(context.Background(), plan.ID); err == nil {
		t.Fatal("expected exhausted budget error")
	}
	finalPlan, _ := planStore.GetPlan(plan.ID)
	if finalPlan.Status != models.StatusCanceled || finalPlan.Usage.TaskAttempts != 1 {
		t.Fatalf("parallel budget exhaustion left plan active: status=%s attempts=%d", finalPlan.Status, finalPlan.Usage.TaskAttempts)
	}
}

func TestReassignTaskUnblocksDependents(t *testing.T) {
	planStore := store.NewMemoryPlanStore()
	plan := governanceTestPlan("reassign-dependent-plan", 2)
	plan.Status = models.StatusFailed
	plan.Nodes[0].Status = models.StatusFailed
	plan.Nodes[1].Status = models.StatusBlocked
	plan.Nodes[1].Dependencies = []string{plan.Nodes[0].ID}
	plan.Edges = []*models.TaskEdge{{From: plan.Nodes[0].ID, To: plan.Nodes[1].ID, Type: "control"}}
	if err := planStore.SavePlan(plan); err != nil {
		t.Fatal(err)
	}
	runner := NewScheduler(planStore, NewDefaultTaskExecutor(), nil, 1)
	if err := runner.ReassignTask(plan.ID, plan.Nodes[0].ID, "coder_agent"); err != nil {
		t.Fatal(err)
	}
	if err := runner.ExecutePlan(context.Background(), plan.ID); err != nil {
		t.Fatal(err)
	}
	finalPlan, _ := planStore.GetPlan(plan.ID)
	if finalPlan.Status != models.StatusCompleted {
		t.Fatalf("reassignment left descendants blocked: status=%s descendant=%s", finalPlan.Status, finalPlan.Nodes[1].Status)
	}
}

type eventPublisherFunc func(string, models.PlanEvent)

func (fn eventPublisherFunc) Publish(planID string, event models.PlanEvent) { fn(planID, event) }

func TestManualRetryCannotBeBlockedByPreviousFailure(t *testing.T) {
	planStore := store.NewMemoryPlanStore()
	plan := governanceTestPlan("retry-failure-race-plan", 2)
	plan.Nodes[1].Dependencies = []string{plan.Nodes[0].ID}
	plan.Edges = []*models.TaskEdge{{From: plan.Nodes[0].ID, To: plan.Nodes[1].ID, Type: "control"}}
	if err := planStore.SavePlan(plan); err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int32
	var runner *Scheduler
	retryErr := make(chan error, 1)
	publisher := eventPublisherFunc(func(_ string, event models.PlanEvent) {
		if event.EventType == "task_failed" && event.TaskID == plan.Nodes[0].ID {
			// Reproduce the API retry arriving after failure is committed/published,
			// but before the previous worker propagates blocked state downstream.
			retryErr <- runner.RetryTask(plan.ID, event.TaskID)
		}
	})
	runner = NewScheduler(planStore, taskExecutorFunc(func(_ context.Context, _ *models.PlanGraph, task *models.TaskNode) (*models.TaskExecutionResult, error) {
		if task.ID == plan.Nodes[0].ID && attempts.Add(1) == 1 {
			return &models.TaskExecutionResult{Status: models.StatusFailed, Error: "first attempt"}, nil
		}
		return &models.TaskExecutionResult{Status: models.StatusCompleted}, nil
	}), publisher, 1)
	if err := runner.ExecutePlan(context.Background(), plan.ID); err != nil {
		t.Fatal(err)
	}
	if err := <-retryErr; err != nil {
		t.Fatal(err)
	}
	finalPlan, _ := planStore.GetPlan(plan.ID)
	if finalPlan.Status != models.StatusCompleted {
		t.Fatalf("old failure blocked manual retry: status=%s descendant=%s", finalPlan.Status, finalPlan.Nodes[1].Status)
	}
}
