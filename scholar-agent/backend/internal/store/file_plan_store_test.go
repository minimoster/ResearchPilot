package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"scholar-agent-backend/internal/models"
)

func TestFilePlanStorePersistsPlansAndEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "plans.json")
	plan := testPlan("persisted-plan")
	plan.TraceID = "trace-1"
	plan.Nodes[0].Contract = models.TaskContract{
		Version:         models.TaskContractVersion,
		InputArtifacts:  []string{"paper"},
		OutputArtifacts: []string{"report"},
	}

	first, err := NewFilePlanStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.SavePlan(plan); err != nil {
		t.Fatal(err)
	}
	if err := first.AppendEvent(plan.ID, models.PlanEvent{PlanID: plan.ID, TraceID: plan.TraceID, EventType: "plan_created"}); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewFilePlanStore(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reopened.GetPlan(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.TraceID != "trace-1" || loaded.Nodes[0].Contract.Version != models.TaskContractVersion {
		t.Fatalf("runtime metadata was not persisted: %#v", loaded)
	}
	events, err := reopened.ListEvents(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventType != "plan_created" {
		t.Fatalf("events were not persisted: %#v", events)
	}

	loaded.Nodes[0].Name = "mutated outside store"
	again, _ := reopened.GetPlan(plan.ID)
	if again.Nodes[0].Name == loaded.Nodes[0].Name {
		t.Fatal("GetPlan must return a deep clone")
	}
}

func TestRecoverInterruptedPlansInvalidatesExecutionLease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plans.json")
	planStore, err := NewFilePlanStore(path)
	if err != nil {
		t.Fatal(err)
	}
	plan := testPlan("interrupted-plan")
	now := time.Now()
	plan.Status = models.StatusInProgress
	plan.Nodes[0].Status = models.StatusInProgress
	plan.Nodes[0].ExecutionID = "old-execution"
	plan.Nodes[0].LeaseOwner = "coder-agent"
	plan.Nodes[0].LeaseExpiresAt = &now
	if err := planStore.SavePlan(plan); err != nil {
		t.Fatal(err)
	}

	if err := RecoverInterruptedPlans(planStore); err != nil {
		t.Fatal(err)
	}
	recovered, err := planStore.GetPlan(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != models.StatusPending || recovered.Nodes[0].Status != models.StatusPending {
		t.Fatalf("interrupted work was not made runnable: plan=%s task=%s", recovered.Status, recovered.Nodes[0].Status)
	}
	if recovered.Nodes[0].ExecutionID != "" || recovered.Nodes[0].LeaseOwner != "" || recovered.Nodes[0].LeaseExpiresAt != nil {
		t.Fatalf("stale lease was not invalidated: %#v", recovered.Nodes[0])
	}
}

func testPlan(id string) *models.PlanGraph {
	now := time.Now()
	return &models.PlanGraph{
		ID:        id,
		Status:    models.StatusPending,
		CreatedAt: now,
		UpdatedAt: now,
		Nodes: []*models.TaskNode{{
			ID:         "task-1",
			Name:       "Run experiment",
			Status:     models.StatusPending,
			AssignedTo: "coder-agent",
			CreatedAt:  now,
			UpdatedAt:  now,
		}},
		Artifacts: map[string]models.Artifact{},
	}
}

func TestFilePlanStoreFailedWritesDoNotChangeMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plans.json")
	planStore, err := NewFilePlanStore(path)
	if err != nil {
		t.Fatal(err)
	}
	plan := testPlan("existing")
	if err := planStore.SavePlan(plan); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := planStore.UpdatePlan(plan.ID, func(current *models.PlanGraph) error {
		current.Status = models.StatusCompleted
		return nil
	}); err == nil {
		t.Fatal("expected replacement of a directory to fail")
	}
	loaded, _ := planStore.GetPlan(plan.ID)
	if loaded.Status != models.StatusPending {
		t.Fatalf("failed update leaked into memory: %s", loaded.Status)
	}
	if err := planStore.AppendEvent(plan.ID, models.PlanEvent{EventType: "uncommitted"}); err == nil {
		t.Fatal("expected append persistence to fail")
	}
	events, _ := planStore.ListEvents(plan.ID)
	if len(events) != 0 {
		t.Fatalf("failed append leaked into memory: %#v", events)
	}
	if err := planStore.SavePlan(testPlan("new")); err == nil {
		t.Fatal("expected save persistence to fail")
	}
	if _, err := planStore.GetPlan("new"); err == nil {
		t.Fatal("failed save leaked into memory")
	}
}

func TestPlanStoresIsolateNestedValues(t *testing.T) {
	fileStore, err := NewFilePlanStore(filepath.Join(t.TempDir(), "plans.json"))
	if err != nil {
		t.Fatal(err)
	}
	for name, planStore := range map[string]PlanStore{"memory": NewMemoryPlanStore(), "file": fileStore} {
		t.Run(name, func(t *testing.T) {
			plan := testPlan("nested")
			plan.Nodes[0].Inputs = map[string]any{"files": []map[string]any{{"name": "original"}}}
			if err := planStore.SavePlan(plan); err != nil {
				t.Fatal(err)
			}
			plan.Nodes[0].Inputs["files"].([]map[string]any)[0]["name"] = "outside mutation"
			loaded, _ := planStore.GetPlan(plan.ID)
			if loaded.Nodes[0].Inputs["files"].([]map[string]any)[0]["name"] != "original" {
				t.Fatal("SavePlan retained a nested alias")
			}
			loaded.Nodes[0].Inputs["files"].([]map[string]any)[0]["name"] = "read mutation"
			if err := planStore.UpdatePlan(plan.ID, func(current *models.PlanGraph) error {
				current.Nodes[0].Inputs["files"].([]map[string]any)[0]["name"] = "rejected mutation"
				return fmt.Errorf("reject update")
			}); err == nil {
				t.Fatal("expected rejected update")
			}
			again, _ := planStore.GetPlan(plan.ID)
			if again.Nodes[0].Inputs["files"].([]map[string]any)[0]["name"] != "original" {
				t.Fatal("GetPlan or rejected UpdatePlan leaked a nested mutation")
			}
			event := models.PlanEvent{Payload: map[string]any{"values": []any{map[string]any{"value": "original"}}}}
			if err := planStore.AppendEvent(plan.ID, event); err != nil {
				t.Fatal(err)
			}
			event.Payload["values"].([]any)[0].(map[string]any)["value"] = "outside mutation"
			history, _ := planStore.ListEvents(plan.ID)
			if history[0].Payload["values"].([]any)[0].(map[string]any)["value"] != "original" {
				t.Fatal("AppendEvent retained nested alias")
			}
		})
	}
}
