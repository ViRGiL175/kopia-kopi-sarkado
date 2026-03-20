package planner

import (
	"testing"
	"time"

	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/kopia"
)

func TestBuildPlanKeepsOldestLatestAndRepresentatives(t *testing.T) {
	now := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	snapshots := []kopia.Snapshot{
		{ID: "s1", EndTime: now.Add(-16 * 24 * time.Hour)},
		{ID: "s2", EndTime: now.Add(-12 * 24 * time.Hour)},
		{ID: "s3", EndTime: now.Add(-8 * 24 * time.Hour)},
		{ID: "s4", EndTime: now.Add(-4 * 24 * time.Hour)},
		{ID: "s5", EndTime: now.Add(-2 * 24 * time.Hour)},
		{ID: "s6", EndTime: now.Add(-1 * 24 * time.Hour)},
		{ID: "s7", EndTime: now},
	}

	plan := BuildPlan(snapshots, Options{KeepLatest: 2})
	if len(plan.Kept) < 4 {
		t.Fatalf("expected multiple kept snapshots, got %d", len(plan.Kept))
	}

	if len(plan.Candidates) == 0 {
		t.Fatal("expected at least one pruning candidate")
	}

	latestFound := false
	oldestFound := false
	for _, kept := range plan.Kept {
		if kept.Snapshot.ID == "s1" {
			oldestFound = true
		}
		if kept.Snapshot.ID == "s7" {
			latestFound = true
		}
	}

	if !oldestFound || !latestFound {
		t.Fatalf("expected oldest and latest snapshots to be kept")
	}
}
