package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/kopia"
	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/planner"
)

type realIntegrationEnv struct {
	repoDir    string
	sourceDir  string
	configFile string
	password   string
	before     []kopia.Snapshot
	client     kopia.Client
}

func TestRunPreflightDeletesUpToMaxPassesWhenStillInsufficient(t *testing.T) {
	t.Parallel()

	ctx, cancel := realIntegrationContext(t)
	defer cancel()

	env := setupRealIntegrationEnv(t, ctx, 10)
	freeBytes := int64(1 << 30)
	estimateBytes, err := env.client.EstimateSnapshotSize(ctx, env.sourceDir)
	if err != nil {
		t.Fatalf("EstimateSnapshotSize() error = %v", err)
	}

	plan := planner.BuildPlan(env.before, planner.Options{KeepLatest: 1})
	if len(plan.Candidates) < 3 {
		t.Fatalf("expected at least 3 candidates, got %d", len(plan.Candidates))
	}

	freeProbe := staticFreeBytes(freeBytes)
	result, code, err := runPreflight(ctx, os.Stdout, Config{
		Source:             env.sourceDir,
		SpacePath:          env.repoDir,
		MaxPasses:          2,
		BatchSize:          1,
		KeepLatest:         1,
		EstimateMultiplier: 1,
		ServiceReserve:     0,
		SafetyMargin:       freeBytes + (2 * estimateBytes),
		RunMaintenance:     false,
		MaintenanceMode:    "quick",
		Apply:              true,
	}, env.client, freeProbe)
	if err != nil {
		t.Fatalf("runPreflight() error = %v", err)
	}

	if code != ExitInsufficientSpace {
		t.Fatalf("runPreflight() code = %d, want %d", code, ExitInsufficientSpace)
	}
	if result.ReadyForBackup {
		t.Fatal("expected backup to remain blocked after max passes")
	}
	if len(result.DeletedIDs) != 2 {
		t.Fatalf("expected exactly 2 deletions, got %d", len(result.DeletedIDs))
	}
	if result.PassesExecuted != 2 {
		t.Fatalf("expected exactly 2 executed passes, got %d", result.PassesExecuted)
	}

	afterSnapshots, err := env.client.ListSnapshots(ctx, env.sourceDir)
	if err != nil {
		t.Fatalf("ListSnapshots() after preflight error = %v", err)
	}
	if len(afterSnapshots) != len(env.before)-2 {
		t.Fatalf("expected two deleted snapshots, before=%d after=%d", len(env.before), len(afterSnapshots))
	}
}

func TestRunPreflightStopsAtRetentionFloorWhenStillInsufficient(t *testing.T) {
	t.Parallel()

	ctx, cancel := realIntegrationContext(t)
	defer cancel()

	env := setupRealIntegrationEnv(t, ctx, 10)
	freeBytes := int64(1 << 30)
	estimateBytes, err := env.client.EstimateSnapshotSize(ctx, env.sourceDir)
	if err != nil {
		t.Fatalf("EstimateSnapshotSize() error = %v", err)
	}

	plan := planner.BuildPlan(env.before, planner.Options{KeepLatest: 1})
	optimisticAll := estimateBytes * int64(len(plan.Candidates))
	if optimisticAll <= 1 {
		t.Fatalf("expected optimistic reclaim > 1, got %d", optimisticAll)
	}

	result, code, err := runPreflight(ctx, os.Stdout, Config{
		Source:             env.sourceDir,
		SpacePath:          env.repoDir,
		MaxPasses:          10,
		BatchSize:          10,
		KeepLatest:         1,
		EstimateMultiplier: 1,
		ServiceReserve:     0,
		SafetyMargin:       freeBytes + optimisticAll - estimateBytes - 1,
		RunMaintenance:     false,
		MaintenanceMode:    "quick",
		Apply:              true,
	}, env.client, staticFreeBytes(freeBytes))
	if err != nil {
		t.Fatalf("runPreflight() error = %v", err)
	}

	if code != ExitInsufficientSpace {
		t.Fatalf("runPreflight() code = %d, want %d", code, ExitInsufficientSpace)
	}
	if result.ReadyForBackup {
		t.Fatal("expected backup to remain blocked at the retention floor")
	}

	afterSnapshots, err := env.client.ListSnapshots(ctx, env.sourceDir)
	if err != nil {
		t.Fatalf("ListSnapshots() after preflight error = %v", err)
	}
	if len(afterSnapshots) != len(result.Plan.Kept) {
		t.Fatalf("expected to stop at retention floor, kept=%d after=%d", len(result.Plan.Kept), len(afterSnapshots))
	}
	if len(result.DeletedIDs)+len(afterSnapshots) != len(env.before) {
		t.Fatalf("expected deleted + kept to match original snapshots, before=%d deleted=%d after=%d", len(env.before), len(result.DeletedIDs), len(afterSnapshots))
	}

	keptIDs := decisionIDs(result.Plan.Kept)
	afterIDs := snapshotIDs(afterSnapshots)
	for _, id := range keptIDs {
		if !slices.Contains(afterIDs, id) {
			t.Fatalf("expected kept snapshot %s to remain after preflight", id)
		}
	}
}

func realIntegrationContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()

	if _, err := exec.LookPath("kopia"); err != nil {
		t.Skip("kopia CLI not found in PATH")
	}

	return context.WithTimeout(context.Background(), 2*time.Minute)
}

func setupRealIntegrationEnv(t *testing.T, ctx context.Context, snapshotCount int) realIntegrationEnv {
	t.Helper()

	rootDir := t.TempDir()
	repoDir := filepath.Join(rootDir, "repo")
	sourceDir := filepath.Join(rootDir, "source")
	configFile := filepath.Join(rootDir, "repository.config")
	password := "integration-secret"

	mustMkdirAll(t, repoDir)
	mustMkdirAll(t, sourceDir)

	runKopia(t, ctx, password,
		"repository", "create", "filesystem",
		"--path", repoDir,
		"--config-file", configFile,
		"--password", password,
		"--no-progress",
	)

	for i := 0; i < snapshotCount; i++ {
		content := strings.Repeat(fmt.Sprintf("snapshot-%d-", i), 1024*8)
		mustWriteFile(t, filepath.Join(sourceDir, "data.txt"), []byte(content))
		runKopia(t, ctx, password,
			"snapshot", "create",
			"--config-file", configFile,
			"--password", password,
			"--no-progress",
			sourceDir,
		)
	}

	client := kopia.Client{Binary: "kopia", ConfigFile: configFile, Password: password}
	beforeSnapshots, err := client.ListSnapshots(ctx, sourceDir)
	if err != nil {
		t.Fatalf("ListSnapshots() before preflight error = %v", err)
	}
	if len(beforeSnapshots) < snapshotCount {
		t.Fatalf("expected at least %d snapshots, got %d", snapshotCount, len(beforeSnapshots))
	}

	return realIntegrationEnv{
		repoDir:    repoDir,
		sourceDir:  sourceDir,
		configFile: configFile,
		password:   password,
		before:     beforeSnapshots,
		client:     client,
	}
}

func staticFreeBytes(free int64) freeBytesFunc {
	return func(path string) (int64, error) {
		return free, nil
	}
}

func decisionIDs(decisions []planner.Decision) []string {
	ids := make([]string, 0, len(decisions))
	for _, decision := range decisions {
		ids = append(ids, decision.Snapshot.ID)
	}
	return ids
}

func snapshotIDs(snapshots []kopia.Snapshot) []string {
	ids := make([]string, 0, len(snapshots))
	for _, snapshot := range snapshots {
		ids = append(ids, snapshot.ID)
	}
	return ids
}

func runKopia(t *testing.T, ctx context.Context, password string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(ctx, "kopia", args...)
	cmd.Env = append(cmd.Environ(), "KOPIA_PASSWORD="+password)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kopia %s failed: %v\n%s", strings.Join(args, " "), err, string(output))
	}

	return string(output)
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}
