package store

import "scholar-agent-backend/internal/models"

// PlanStore persists plan graphs and their event history.
type PlanStore interface {
	SavePlan(plan *models.PlanGraph) error
	GetPlan(planID string) (*models.PlanGraph, error)
	ListPlans() ([]*models.PlanGraph, error)
	UpdatePlan(planID string, update func(*models.PlanGraph) error) error
	// UpdatePlanWithEvents commits the update and its event batch together.
	// The callback runs under the store lock and must not call store methods.
	UpdatePlanWithEvents(planID string, update func(*models.PlanGraph) ([]models.PlanEvent, error)) error
	AppendEvent(planID string, event models.PlanEvent) error
	ListEvents(planID string) ([]models.PlanEvent, error)
}
