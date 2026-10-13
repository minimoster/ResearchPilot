package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"scholar-agent-backend/internal/models"
	"scholar-agent-backend/internal/store"
)

var errEventPersistence = errors.New("injected event persistence failure")

type failingEventStore struct{ store.PlanStore }

func (s failingEventStore) AppendEvent(string, models.PlanEvent) error { return errEventPersistence }
func (s failingEventStore) UpdatePlanWithEvents(string, func(*models.PlanGraph) ([]models.PlanEvent, error)) error {
	return errEventPersistence
}

func TestFailedEventPersistenceDoesNotPublish(t *testing.T) {
	planStore := store.NewMemoryPlanStore()
	plan := governanceTestPlan("event-failure", 1)
	if err := planStore.SavePlan(plan); err != nil {
		t.Fatal(err)
	}
	published := 0
	runner := NewScheduler(failingEventStore{planStore}, nil, eventPublisherFunc(func(string, models.PlanEvent) { published++ }), 1)
	if err := runner.publishAndStore(plan.ID, models.PlanEvent{EventType: "task_log"}); !errors.Is(err, errEventPersistence) {
		t.Fatalf("append error = %v", err)
	}
	if published != 0 {
		t.Fatalf("published %d events whose persistence failed", published)
	}
}

func TestCancelPlanEventFailureLeavesStateUnchanged(t *testing.T) {
	planStore := store.NewMemoryPlanStore()
	plan := governanceTestPlan("cancel-event-failure", 1)
	if err := planStore.SavePlan(plan); err != nil {
		t.Fatal(err)
	}
	published := 0
	runner := NewScheduler(failingEventStore{planStore}, nil, eventPublisherFunc(func(string, models.PlanEvent) { published++ }), 1)
	if err := runner.CancelPlan(plan.ID, "test"); !errors.Is(err, errEventPersistence) {
		t.Errorf("CancelPlan error = %v, want persistence error", err)
	}
	current, err := planStore.GetPlan(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != plan.Status {
		t.Errorf("failed cancellation changed status to %s", current.Status)
	}
	events, err := planStore.ListEvents(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 || published != 0 {
		t.Errorf("failed cancellation events: persisted=%d published=%d", len(events), published)
	}
}

// Reject after running the transition on the store's working copy, matching
// a disk failure after state and events have been constructed.
type rejectingTransitionStore struct{ store.PlanStore }

func (s rejectingTransitionStore) UpdatePlanWithEvents(planID string, update func(*models.PlanGraph) ([]models.PlanEvent, error)) error {
	return s.PlanStore.UpdatePlanWithEvents(planID, func(plan *models.PlanGraph) ([]models.PlanEvent, error) {
		events, err := update(plan)
		if err != nil {
			return nil, err
		}
		if len(events) != 0 {
			return events, errEventPersistence
		}
		return events, nil
	})
}

func TestSchedulerFailedTransitionsLeaveStateAndHistoryUnchanged(t *testing.T) {
	for _, name := range []string{"cancel", "retry", "reassign", "plan_start", "task_start", "complete", "fail", "block"} {
		t.Run(name, func(t *testing.T) {
			planStore := store.NewMemoryPlanStore()
			plan := governanceTestPlan("transition-"+name, 2)
			plan.TraceID = "trace-atomic"
			task := plan.Nodes[0]
			task.ExecutionID = "execution"
			task.ExecutionEpoch = 1
			task.LeaseOwner = task.AssignedTo
			task.Status = models.StatusInProgress
			task.RunCount = 1
			if name == "retry" || name == "block" {
				task.Status = models.StatusFailed
			}
			if name == "task_start" {
				task.Status = models.StatusReady
			}
			plan.Nodes[1].Status = models.StatusPending
			plan.Nodes[1].Dependencies = []string{task.ID}
			plan.Edges = []*models.TaskEdge{{From: task.ID, To: plan.Nodes[1].ID, Type: "control"}}
			if err := planStore.SavePlan(plan); err != nil {
				t.Fatal(err)
			}
			before, _ := planStore.GetPlan(plan.ID)
			published := 0
			runner := NewScheduler(rejectingTransitionStore{planStore}, nil, eventPublisherFunc(func(string, models.PlanEvent) { published++ }), 1)
			var err error
			switch name {
			case "cancel":
				err = runner.CancelPlan(plan.ID, "test")
			case "retry":
				err = runner.RetryTask(plan.ID, task.ID)
			case "reassign":
				err = runner.ReassignTask(plan.ID, task.ID, "new-agent")
			case "plan_start":
				err = runner.ExecutePlan(context.Background(), plan.ID)
			case "task_start":
				_, _, err = runner.markTaskStarted(plan.ID, task.ID)
			case "complete":
				err = runner.markTaskCompleted(plan.ID, task.ID, task.ExecutionID, task.LeaseOwner, &models.TaskExecutionResult{Status: models.StatusCompleted, Artifacts: []models.Artifact{{Key: "output", Value: "result"}}})
			case "fail":
				_, err = runner.markTaskFailed(plan.ID, task.ID, task.ExecutionID, task.LeaseOwner, &models.TaskExecutionResult{Status: models.StatusFailed, Error: "test"})
			case "block":
				err = runner.blockDependents(plan.ID, task.ID, task.ExecutionEpoch)
			}
			if !errors.Is(err, errEventPersistence) {
				t.Fatalf("transition error = %v, want persistence error", err)
			}
			after, _ := planStore.GetPlan(plan.ID)
			history, _ := planStore.ListEvents(plan.ID)
			if !reflect.DeepEqual(before, after) || len(history) != 0 || published != 0 {
				t.Fatalf("failed transition changed state=%v, persisted=%d, published=%d", !reflect.DeepEqual(before, after), len(history), published)
			}
		})
	}
}

func TestSchedulerPublishesOnlyAfterBatchIsDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plans.json")
	planStore, err := store.NewFilePlanStore(path)
	if err != nil {
		t.Fatal(err)
	}
	plan := governanceTestPlan("durable-events", 1)
	plan.TraceID = "trace-durable"
	if err := planStore.SavePlan(plan); err != nil {
		t.Fatal(err)
	}
	var published atomic.Int32
	completionPublished := make(chan struct{})
	publisher := eventPublisherFunc(func(_ string, event models.PlanEvent) {
		// Open a fresh reader from disk for every live event, including the first
		// artifact event in the multi-event completion batch.
		reader, err := store.NewFilePlanStore(path)
		if err != nil {
			t.Error(err)
			return
		}
		current, err := reader.GetPlan(plan.ID)
		if err != nil {
			t.Error(err)
			return
		}
		history, err := reader.ListEvents(plan.ID)
		if err != nil {
			t.Error(err)
			return
		}
		found := false
		completion := false
		for _, persisted := range history {
			if persisted.EventType == event.EventType && persisted.TaskID == event.TaskID {
				found = true
			}
			if persisted.EventType == "task_completed" {
				completion = true
			}
		}
		if !found {
			t.Errorf("event %s published before persistence", event.EventType)
		}
		if event.TraceID != plan.TraceID || event.Timestamp.IsZero() || event.PlanID != plan.ID {
			t.Errorf("incomplete event metadata: %#v", event)
		}
		if event.TaskID != "" && event.SpanID != "task_"+event.TaskID {
			t.Errorf("missing task span: %#v", event)
		}
		if event.EventType == "artifact_created" && (!completion || current.Nodes[0].Status != models.StatusCompleted) {
			t.Error("artifact published before entire completion transaction")
		}
		published.Add(1)
		if event.EventType == "task_completed" {
			close(completionPublished)
		}
	})
	runner := NewScheduler(planStore, taskExecutorFunc(func(context.Context, *models.PlanGraph, *models.TaskNode) (*models.TaskExecutionResult, error) {
		return &models.TaskExecutionResult{Status: models.StatusCompleted, Result: "done", Artifacts: []models.Artifact{{Key: "output", Value: "done"}}}, nil
	}), publisher, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runner.ExecutePlan(ctx, plan.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-completionPublished:
	case <-ctx.Done():
		t.Fatal("completion publication did not finish")
	}
	history, err := planStore.ListEvents(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	types := make([]string, 0, len(history))
	for _, event := range history {
		types = append(types, event.EventType)
	}
	want := []string{"plan_started", "task_ready", "task_started", "artifact_created", "task_completed", "plan_completed"}
	if !reflect.DeepEqual(types, want) || int(published.Load()) != len(want) {
		t.Fatalf("history=%v published=%d", types, published.Load())
	}
}

type rejectEventTypeStore struct {
	store.PlanStore
	eventType string
}

func (s rejectEventTypeStore) UpdatePlanWithEvents(planID string, update func(*models.PlanGraph) ([]models.PlanEvent, error)) error {
	return s.PlanStore.UpdatePlanWithEvents(planID, func(plan *models.PlanGraph) ([]models.PlanEvent, error) {
		batch, err := update(plan)
		if err != nil {
			return nil, err
		}
		for _, event := range batch {
			if event.EventType == s.eventType {
				return batch, errEventPersistence
			}
		}
		return batch, nil
	})
}

func TestExecutePlanPropagatesReadinessAndTerminalPersistenceFailure(t *testing.T) {
	for _, eventType := range []string{"task_ready", "plan_completed", "plan_failed"} {
		t.Run(eventType, func(t *testing.T) {
			planStore := store.NewMemoryPlanStore()
			plan := governanceTestPlan("reject-"+eventType, 1)
			switch eventType {
			case "plan_completed":
				plan.Nodes[0].Status = models.StatusCompleted
			case "plan_failed":
				plan.Nodes[0].Dependencies = []string{"missing-dependency"}
			}
			initialTaskStatus := plan.Nodes[0].Status
			if err := planStore.SavePlan(plan); err != nil {
				t.Fatal(err)
			}
			publisher := eventPublisherFunc(func(_ string, event models.PlanEvent) {
				if event.EventType == eventType {
					t.Errorf("published rejected %s event", eventType)
				}
			})
			runner := NewScheduler(rejectEventTypeStore{planStore, eventType}, taskExecutorFunc(func(context.Context, *models.PlanGraph, *models.TaskNode) (*models.TaskExecutionResult, error) {
				t.Error("must not launch tasks for these rejected transitions")
				return &models.TaskExecutionResult{Status: models.StatusCompleted}, nil
			}), publisher, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := runner.ExecutePlan(ctx, plan.ID); !errors.Is(err, errEventPersistence) {
				t.Fatalf("run error = %v", err)
			}
			current, _ := planStore.GetPlan(plan.ID)
			history, _ := planStore.ListEvents(plan.ID)
			if current.Status != models.StatusInProgress || current.Nodes[0].Status != initialTaskStatus || len(history) != 1 || history[0].EventType != "plan_started" {
				t.Fatalf("failed transition leaked: plan=%s task=%s events=%#v", current.Status, current.Nodes[0].Status, history)
			}
		})
	}
}
