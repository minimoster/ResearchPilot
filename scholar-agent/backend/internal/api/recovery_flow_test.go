package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"scholar-agent-backend/internal/events"
	"scholar-agent-backend/internal/models"
	"scholar-agent-backend/internal/planner"
	"scholar-agent-backend/internal/scheduler"
	"scholar-agent-backend/internal/store"
)

// The real HTTP planner, scheduler, durable store and SSE are exercised. Only
// external execution is replaced: channels inject a failed or uncooperative run.
type recoveryExecutor struct {
	mu      sync.Mutex
	calls   map[string]int
	mode    string
	started chan struct{}
	release chan struct{}
}

func (e *recoveryExecutor) ExecuteTask(ctx context.Context, plan *models.PlanGraph, task *models.TaskNode) (*models.TaskExecutionResult, error) {
	e.mu.Lock()
	e.calls[task.ID]++
	attempt := e.calls[task.ID]
	e.mu.Unlock()
	for _, key := range task.RequiredArtifacts {
		if _, ok := plan.Artifacts[key]; !ok {
			return nil, fmt.Errorf("missing input %s", key)
		}
	}
	if task.Type == "execute_code" && attempt == 1 && e.mode != "normal" {
		close(e.started)
		<-e.release // deliberately ignore cancellation to exercise stale result fencing
		if e.mode == "fail" {
			return nil, fmt.Errorf("injected worker failure")
		}
		if logs, ok := ctx.Value("logChannel").(chan string); ok {
			logs <- "STALE LOG"
		}
		return &models.TaskExecutionResult{Status: models.StatusCompleted, Result: "STALE", Artifacts: []models.Artifact{{Key: "stale-output", ProducerTaskID: task.ID, Value: "STALE"}}}, nil
	}
	result := &models.TaskExecutionResult{Status: models.StatusCompleted, Result: "fresh " + task.Type}
	for _, key := range task.OutputArtifacts {
		result.Artifacts = append(result.Artifacts, models.Artifact{Key: key, ProducerTaskID: task.ID, Value: "fresh " + key})
	}
	return result, nil
}

func recoveryRuntime(t *testing.T, ps store.PlanStore, exec *recoveryExecutor) (*planRuntime, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	bus := events.NewBus()
	rt := newPlanRuntime(planner.NewPlanner(), ps, scheduler.NewScheduler(ps, exec, bus, 2), bus)
	router := gin.New()
	rt.register(router.Group("/api"))
	return rt, router
}
func recoveryRequest(t *testing.T, router *gin.Engine, method, path, body string, code int) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(userIDHeaderName, "recovery-owner")
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)
	if res.Code != code {
		t.Fatalf("%s %s: got %d want %d: %s", method, path, res.Code, code, res.Body.String())
	}
	return res
}
func recoveryWait(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for recovery state")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func recoveryIdle(t *testing.T, rt *planRuntime, id string) {
	t.Helper()
	recoveryWait(t, func() bool { rt.mu.Lock(); defer rt.mu.Unlock(); _, active := rt.running[id]; return !active })
}
func recoveryStatus(t *testing.T, ps store.PlanStore, id string, status models.TaskStatus) *models.PlanGraph {
	t.Helper()
	var current *models.PlanGraph
	recoveryWait(t, func() bool {
		var err error
		current, err = ps.GetPlan(id)
		return err == nil && current.Status == status
	})
	return current
}
func recoveryCreate(t *testing.T, router *gin.Engine, ps store.PlanStore) *models.PlanGraph {
	t.Helper()
	response := recoveryRequest(t, router, "POST", "/api/plan", `{"intent":"运行一段 Python 代码并分析结果"}`, 200)
	var out struct {
		Plan *models.PlanGraph `json:"plan_graph"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if err := ps.UpdatePlan(out.Plan.ID, func(p *models.PlanGraph) error {
		for _, task := range p.Nodes {
			task.RetryLimit = 0
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out.Plan
}
func recoveryHasEvent(ps store.PlanStore, id, kind string) bool {
	history, _ := ps.ListEvents(id)
	for _, event := range history {
		if event.EventType == kind {
			return true
		}
	}
	return false
}

func TestHTTPFailureRetryDeliversNewTerminalAndPreservesUpstream(t *testing.T) {
	for _, scope := range []string{"plan", "task"} {
		t.Run(scope, func(t *testing.T) {
			ps, err := store.NewFilePlanStore(filepath.Join(t.TempDir(), "plans.json"))
			if err != nil {
				t.Fatal(err)
			}
			exec := &recoveryExecutor{calls: map[string]int{}, mode: "fail", started: make(chan struct{}), release: make(chan struct{})}
			close(exec.release)
			rt, router := recoveryRuntime(t, ps, exec)
			plan := recoveryCreate(t, router, ps)
			base := "/api/plans/" + plan.ID
			recoveryRequest(t, router, "POST", base+"/execute", "", 202)
			failed := recoveryStatus(t, ps, plan.ID, models.StatusFailed)
			recoveryIdle(t, rt, plan.ID)
			completed := map[string]int{}
			for _, task := range failed.Nodes {
				if task.Status == models.StatusCompleted {
					completed[task.ID] = task.RunCount
				}
			}
			if len(completed) == 0 {
				t.Fatal("expected preserved upstream work")
			}
			retryURL := base + "/retry"
			if scope == "task" {
				for _, task := range failed.Nodes {
					if task.Status == models.StatusFailed {
						retryURL = base + "/tasks/" + task.ID + "/retry"
						break
					}
				}
			}
			recoveryRequest(t, router, "POST", retryURL, "", 200)
			recoveryRequest(t, router, "POST", retryURL, "", 409)
			// Open the new stream before execute: old terminal history must not close it.
			streamDone := make(chan *httptest.ResponseRecorder, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			go func() {
				req := httptest.NewRequest("GET", base+"/stream", nil).WithContext(ctx)
				req.Header.Set(userIDHeaderName, "recovery-owner")
				res := httptest.NewRecorder()
				router.ServeHTTP(res, req)
				streamDone <- res
			}()
			recoveryRequest(t, router, "POST", base+"/execute", "", 202)
			final := recoveryStatus(t, ps, plan.ID, models.StatusCompleted)
			recoveryIdle(t, rt, plan.ID)
			select {
			case stream := <-streamDone:
				text := stream.Body.String()
				if !strings.Contains(text, `"event_type":"plan_completed"`) || strings.Contains(text, `"event_type":"plan_failed"`) {
					t.Fatalf("wrong retry stream: %s", text)
				}
			case <-ctx.Done():
				t.Fatal("retry SSE did not reach terminal")
			}
			for _, task := range final.Nodes {
				if count, ok := completed[task.ID]; ok && task.RunCount != count {
					t.Fatal("completed upstream reran")
				}
			}
			if !recoveryHasEvent(ps, plan.ID, "plan_failed") {
				t.Fatal("audit history lost prior failure")
			}
			recoveryRequest(t, router, "POST", base+"/execute", "", 409)
		})
	}
}

func TestHTTPCancelRetryDiscardsLateArtifacts(t *testing.T) {
	ps := store.NewMemoryPlanStore()
	exec := &recoveryExecutor{calls: map[string]int{}, mode: "late", started: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(exec.release) }) }
	defer release()
	rt, router := recoveryRuntime(t, ps, exec)
	plan := recoveryCreate(t, router, ps)
	base := "/api/plans/" + plan.ID
	recoveryRequest(t, router, "POST", base+"/execute", "", 202)
	select {
	case <-exec.started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker not started")
	}
	recoveryRequest(t, router, "POST", base+"/execute", "", 409)
	recoveryRequest(t, router, "POST", base+"/retry", "", 409)
	var taskID string
	for _, task := range plan.Nodes {
		if task.Type == "execute_code" {
			taskID = task.ID
		}
	}
	recoveryRequest(t, router, "POST", base+"/tasks/"+taskID+"/reassign", `{"assigned_to":"other"}`, 409)
	recoveryRequest(t, router, "POST", base+"/cancel", "", 200)
	recoveryRequest(t, router, "POST", base+"/cancel", "", 409)
	recoveryIdle(t, rt, plan.ID)
	recoveryRequest(t, router, "POST", base+"/retry", "", 200)
	recoveryRequest(t, router, "POST", base+"/execute", "", 202)
	recoveryStatus(t, ps, plan.ID, models.StatusCompleted)
	recoveryIdle(t, rt, plan.ID)
	release()
	recoveryWait(t, func() bool { return recoveryHasEvent(ps, plan.ID, "task_result_discarded") })
	history, _ := ps.ListEvents(plan.ID)
	for _, event := range history {
		if event.EventType == "task_log" && event.Payload["message"] == "STALE LOG" {
			t.Fatal("late log accepted")
		}
	}
	final, _ := ps.GetPlan(plan.ID)
	if _, ok := final.Artifacts["stale-output"]; ok {
		t.Fatal("late artifact accepted")
	}
	for _, task := range final.Nodes {
		if task.Result == "STALE" || task.ExecutionID != "" {
			t.Fatalf("stale task state: %+v", task)
		}
	}
}

func TestHTTPDeadlineLeavesRetryableState(t *testing.T) {
	ps := store.NewMemoryPlanStore()
	exec := &recoveryExecutor{calls: map[string]int{}, mode: "late", started: make(chan struct{}), release: make(chan struct{})}
	defer close(exec.release)
	rt, router := recoveryRuntime(t, ps, exec)
	plan := recoveryCreate(t, router, ps)
	base := "/api/plans/" + plan.ID
	if err := ps.UpdatePlan(plan.ID, func(p *models.PlanGraph) error { p.Budget.MaxDurationSec = 2; return nil }); err != nil {
		t.Fatal(err)
	}
	recoveryRequest(t, router, "POST", base+"/execute", "", 202)
	canceled := recoveryStatus(t, ps, plan.ID, models.StatusCanceled)
	recoveryIdle(t, rt, plan.ID)
	if canceled.Usage.FinishedAt == nil || canceled.Meta.InProgressNodes != 0 {
		t.Fatal("interrupted run not finalized")
	}
	for _, task := range canceled.Nodes {
		if task.ExecutionID != "" || task.LeaseExpiresAt != nil {
			t.Fatal("interrupted lease retained")
		}
	}
	recoveryRequest(t, router, "POST", base+"/retry", "", 200)
	recoveryRequest(t, router, "POST", base+"/execute", "", 202)
	recoveryStatus(t, ps, plan.ID, models.StatusCompleted)
	recoveryIdle(t, rt, plan.ID)
}

func TestHTTPReopensInterruptedSnapshotAndResumes(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "plans.json")
	ps, err := store.NewFilePlanStore(path)
	if err != nil {
		t.Fatal(err)
	}
	exec := &recoveryExecutor{calls: map[string]int{}, mode: "late", started: make(chan struct{}), release: make(chan struct{})}
	defer close(exec.release)
	rt, router := recoveryRuntime(t, ps, exec)
	plan := recoveryCreate(t, router, ps)
	base := "/api/plans/" + plan.ID
	recoveryRequest(t, router, "POST", base+"/execute", "", 202)
	select {
	case <-exec.started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker not started")
	}
	// Capture a real durable in-progress snapshot, then retire the old runtime.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	recoveryRequest(t, router, "POST", base+"/cancel", "", 200)
	recoveryIdle(t, rt, plan.ID)
	recoveredPath := filepath.Join(directory, "reopened.json")
	if err := os.WriteFile(recoveredPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.NewFilePlanStore(recoveredPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecoverInterruptedPlans(reopened); err != nil {
		t.Fatal(err)
	}
	fresh := &recoveryExecutor{calls: map[string]int{}, mode: "normal"}
	next, nextRouter := recoveryRuntime(t, reopened, fresh)
	recoveryRequest(t, nextRouter, "POST", base+"/execute", "", 202)
	final := recoveryStatus(t, reopened, plan.ID, models.StatusCompleted)
	recoveryIdle(t, next, plan.ID)
	fresh.mu.Lock()
	defer fresh.mu.Unlock()
	for _, task := range final.Nodes {
		if task.Type == "generate_code" && fresh.calls[task.ID] != 0 {
			t.Fatal("recovery reran completed source generation")
		}
	}
}

func TestHTTPRetryResetsAllBranchesAndOutputState(t *testing.T) {
	ps := store.NewMemoryPlanStore()
	exec := &recoveryExecutor{calls: map[string]int{}, mode: "normal"}
	rt, router := recoveryRuntime(t, ps, exec)
	plan := recoveryCreate(t, router, ps)
	base := "/api/plans/" + plan.ID
	// Inject a persisted canceled graph with successful upstream work, multiple
	// unfinished branches and residual output fields from an abandoned attempt.
	if err := ps.UpdatePlan(plan.ID, func(p *models.PlanGraph) error {
		p.Status = models.StatusCanceled
		for i, task := range p.Nodes {
			task.Status = models.StatusCanceled
			task.Result = "old"
			task.Code = "old code"
			task.StructuredData = "old data"
			task.ImageBase64 = "old image"
			if i == 0 {
				task.Status = models.StatusCompleted
				task.RunCount = 1
			}
			for _, key := range task.OutputArtifacts {
				p.Artifacts[key] = models.Artifact{Key: key, ProducerTaskID: task.ID, Value: "old"}
			}
		}
		p.Nodes = append(p.Nodes, &models.TaskNode{ID: "independent-branch", Type: "extra", AssignedTo: "test", Status: models.StatusCanceled, OutputArtifacts: []string{"extra-output"}})
		p.Artifacts["user-input"] = models.Artifact{Key: "user-input", Value: "preserve me"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	response := recoveryRequest(t, router, "POST", base+"/retry", "", 200)
	var reset struct {
		Plan *models.PlanGraph `json:"plan_graph"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &reset); err != nil {
		t.Fatal(err)
	}
	if reset.Plan.Nodes[0].Status != models.StatusCompleted {
		t.Fatal("lost completed upstream")
	}
	for _, task := range reset.Plan.Nodes[1:] {
		if task.Status != models.StatusPending || task.Result != "" || task.Code != "" || task.StructuredData != "" || task.ImageBase64 != "" {
			t.Fatalf("stale output after retry: %+v", task)
		}
		for _, key := range task.OutputArtifacts {
			if _, ok := reset.Plan.Artifacts[key]; ok {
				t.Fatalf("stale artifact %s survived", key)
			}
		}
	}
	if reset.Plan.Artifacts["user-input"].Value != "preserve me" {
		t.Fatal("lost external input")
	}
	recoveryRequest(t, router, "POST", base+"/execute", "", 202)
	final := recoveryStatus(t, ps, plan.ID, models.StatusCompleted)
	recoveryIdle(t, rt, plan.ID)
	if final.Meta.CompletedNodes != len(final.Nodes) {
		t.Fatal("not every branch recovered")
	}
}

func TestHTTPRecoveryMutationsRejectDrainingExecution(t *testing.T) {
	ps := store.NewMemoryPlanStore()
	exec := &recoveryExecutor{calls: map[string]int{}, mode: "normal"}
	rt, router := recoveryRuntime(t, ps, exec)
	plan := recoveryCreate(t, router, ps)
	base := "/api/plans/" + plan.ID
	recoveryRequest(t, router, "POST", base+"/cancel", "", 200)
	// A canceled persisted plan can still have its old loop registered until the
	// goroutine exits. Exercise that window without timing-dependent sleeps.
	rt.mu.Lock()
	rt.running[plan.ID] = func() {}
	rt.mu.Unlock()
	recoveryRequest(t, router, "POST", base+"/retry", "", 409)
	recoveryRequest(t, router, "POST", base+"/tasks/"+plan.Nodes[0].ID+"/retry", "", 409)
	recoveryRequest(t, router, "POST", base+"/tasks/"+plan.Nodes[0].ID+"/reassign", `{"assigned_to":"other"}`, 409)
	rt.mu.Lock()
	delete(rt.running, plan.ID)
	rt.mu.Unlock()
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			req := httptest.NewRequest("POST", base+"/retry", nil)
			req.Header.Set(userIDHeaderName, "recovery-owner")
			res := httptest.NewRecorder()
			router.ServeHTTP(res, req)
			codes <- res.Code
		}()
	}
	first, second := <-codes, <-codes
	if !((first == 200 && second == 409) || (first == 409 && second == 200)) {
		t.Fatalf("duplicate retry statuses: %d, %d", first, second)
	}
}
