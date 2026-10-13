package store

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"scholar-agent-backend/internal/models"
)

func TestPlanStoreTransitionCommitsStateAndEventBatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plans.json")
	fileStore, err := NewFilePlanStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, planStore := range map[string]PlanStore{"memory": NewMemoryPlanStore(), "file": fileStore} {
		t.Run(name, func(t *testing.T) {
			plan := testPlan("atomic")
			if err := planStore.SavePlan(plan); err != nil {
				t.Fatal(err)
			}
			payload := map[string]any{"details": []any{"original"}}
			if err := planStore.UpdatePlanWithEvents(plan.ID, func(current *models.PlanGraph) ([]models.PlanEvent, error) {
				current.Status = models.StatusCompleted
				return []models.PlanEvent{{PlanID: plan.ID, EventType: "artifact_created", Payload: payload}, {PlanID: plan.ID, EventType: "task_completed"}}, nil
			}); err != nil {
				t.Fatal(err)
			}
			payload["details"].([]any)[0] = "outside mutation"
			check := func(reader PlanStore) {
				current, err := reader.GetPlan(plan.ID)
				if err != nil {
					t.Fatal(err)
				}
				history, err := reader.ListEvents(plan.ID)
				if err != nil {
					t.Fatal(err)
				}
				if current.Status != models.StatusCompleted || len(history) != 2 || history[0].EventType != "artifact_created" || history[1].EventType != "task_completed" {
					t.Fatalf("incomplete transition: status=%s history=%#v", current.Status, history)
				}
				if history[0].Payload["details"].([]any)[0] != "original" {
					t.Fatal("transition retained mutable event payload")
				}
			}
			check(planStore)
			if name == "file" {
				reopened, err := NewFilePlanStore(path)
				if err != nil {
					t.Fatal(err)
				}
				check(reopened)
			}
		})
	}
}

func TestPlanStoreRejectedTransitionRollsBackStateAndEvents(t *testing.T) {
	fileStore, err := NewFilePlanStore(filepath.Join(t.TempDir(), "plans.json"))
	if err != nil {
		t.Fatal(err)
	}
	for name, planStore := range map[string]PlanStore{"memory": NewMemoryPlanStore(), "file": fileStore} {
		t.Run(name, func(t *testing.T) {
			plan := testPlan("rejected")
			if err := planStore.SavePlan(plan); err != nil {
				t.Fatal(err)
			}
			rejected := errors.New("reject transition")
			err := planStore.UpdatePlanWithEvents(plan.ID, func(current *models.PlanGraph) ([]models.PlanEvent, error) {
				current.Status = models.StatusCompleted
				return []models.PlanEvent{{EventType: "uncommitted"}}, rejected
			})
			if !errors.Is(err, rejected) {
				t.Fatalf("error = %v", err)
			}
			current, _ := planStore.GetPlan(plan.ID)
			history, _ := planStore.ListEvents(plan.ID)
			if current.Status != models.StatusPending || len(history) != 0 {
				t.Fatalf("rejected transition changed state: %s %#v", current.Status, history)
			}
			called := false
			if err := planStore.UpdatePlanWithEvents("missing", func(*models.PlanGraph) ([]models.PlanEvent, error) { called = true; return nil, nil }); err == nil || called {
				t.Fatal("missing plan must fail without invoking callback")
			}
		})
	}
}

func TestFilePlanStoreFailedTransitionRollsBackStateAndEvents(t *testing.T) {
	for _, failure := range []string{"encode", "replace"} {
		t.Run(failure, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "plans.json")
			planStore, err := NewFilePlanStore(path)
			if err != nil {
				t.Fatal(err)
			}
			plan := testPlan("failed")
			if err := planStore.SavePlan(plan); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if failure == "encode" {
				payload = map[string]any{"invalid": make(chan int)}
			} else {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			err = planStore.UpdatePlanWithEvents(plan.ID, func(current *models.PlanGraph) ([]models.PlanEvent, error) {
				current.Status = models.StatusCompleted
				return []models.PlanEvent{{EventType: "uncommitted", Payload: payload}}, nil
			})
			if err == nil {
				t.Fatal("expected persistence failure")
			}
			current, _ := planStore.GetPlan(plan.ID)
			history, _ := planStore.ListEvents(plan.ID)
			if current.Status != models.StatusPending || len(history) != 0 {
				t.Fatalf("failed persistence changed memory: %s %#v", current.Status, history)
			}
			if failure == "encode" {
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(after) != string(before) {
					t.Fatal("failed transition changed disk snapshot")
				}
			}
			tempFiles, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".plans-*.tmp"))
			if err != nil || len(tempFiles) != 0 {
				t.Fatalf("temporary snapshots leaked: %v %v", tempFiles, err)
			}
		})
	}
}

func TestPlanStoreConcurrentTransitionsRetainEveryEvent(t *testing.T) {
	fileStore, err := NewFilePlanStore(filepath.Join(t.TempDir(), "plans.json"))
	if err != nil {
		t.Fatal(err)
	}
	for name, planStore := range map[string]PlanStore{"memory": NewMemoryPlanStore(), "file": fileStore} {
		t.Run(name, func(t *testing.T) {
			plan := testPlan("concurrent")
			if err := planStore.SavePlan(plan); err != nil {
				t.Fatal(err)
			}
			const count = 24
			var workers sync.WaitGroup
			for i := 0; i < count; i++ {
				workers.Go(func() {
					if err := planStore.UpdatePlanWithEvents(plan.ID, func(current *models.PlanGraph) ([]models.PlanEvent, error) {
						current.Usage.TaskAttempts++
						return []models.PlanEvent{{EventType: "attempt", Payload: map[string]any{"attempt": current.Usage.TaskAttempts}}}, nil
					}); err != nil {
						t.Error(err)
					}
				})
			}
			workers.Wait()
			current, _ := planStore.GetPlan(plan.ID)
			history, _ := planStore.ListEvents(plan.ID)
			if current.Usage.TaskAttempts != count || len(history) != count {
				t.Fatalf("lost transition: attempts=%d history=%d", current.Usage.TaskAttempts, len(history))
			}
			for i, event := range history {
				if event.Payload["attempt"] != i+1 {
					t.Fatalf("history out of commit order: event %d = %#v", i, event)
				}
			}
		})
	}
}
