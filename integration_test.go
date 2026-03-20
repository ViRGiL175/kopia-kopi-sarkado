package main

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

	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/app"
	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/kopia"
	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/planner"
	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/storage"
)

const impossibleSafetyMargin = int64(1 << 30)

type integrationEnv struct {
	repoDir    string
	sourceDir  string
	configFile string
	password   string
	before     []kopia.Snapshot
	beforeFree int64
	client     kopia.Client
}

func TestPreflightIntegrationReadyWithoutDeletion(t *testing.T) {
	t.Parallel()

	ctx, cancel := integrationContext(t)
	defer cancel()

	env := setupIntegrationEnv(t, ctx, 3)

	result, code, err := app.RunPreflight(ctx, os.Stdout, app.Config{
		Source:             env.sourceDir,
		SpacePath:          env.repoDir,
		KopiaBinary:        "kopia",
		KopiaConfigFile:    env.configFile,
		KopiaPassword:      env.password,
		MaxPasses:          3,
		BatchSize:          2,
		KeepLatest:         1,
		EstimateMultiplier: 1,
		ServiceReserve:     0,
		SafetyMargin:       0,
		RunMaintenance:     false,
		MaintenanceMode:    "quick",
		Apply:              true,
	})
	if err != nil {
		t.Fatalf("RunPreflight() error = %v", err)
	}

	if code != app.ExitOK {
		t.Fatalf("RunPreflight() code = %d, want %d", code, app.ExitOK)
	}
	if !result.ReadyForBackup {
		t.Fatal("expected backup to be allowed when free space is already sufficient")
	}
	if len(result.DeletedIDs) != 0 {
		t.Fatalf("expected no deletions, got %d", len(result.DeletedIDs))
	}
	if result.PassesExecuted != 0 {
		t.Fatalf("expected zero passes executed, got %d", result.PassesExecuted)
	}

	afterSnapshots, err := env.client.ListSnapshots(ctx, env.sourceDir)
	if err != nil {
		t.Fatalf("ListSnapshots() after preflight error = %v", err)
	}
	if len(afterSnapshots) != len(env.before) {
		t.Fatalf("expected snapshot count to stay unchanged, before=%d after=%d", len(env.before), len(afterSnapshots))
	}
}

func TestPreflightIntegrationWithRealKopiaRepo(t *testing.T) {
	t.Parallel()

	ctx, cancel := integrationContext(t)
	defer cancel()

	env := setupIntegrationEnv(t, ctx, 6)

	result, code, err := app.RunPreflight(ctx, os.Stdout, app.Config{
		Source:             env.sourceDir,
		SpacePath:          env.repoDir,
		KopiaBinary:        "kopia",
		KopiaConfigFile:    env.configFile,
		KopiaPassword:      env.password,
		MaxPasses:          2,
		BatchSize:          1,
		KeepLatest:         1,
		EstimateMultiplier: 1,
		ServiceReserve:     0,
		SafetyMargin:       env.beforeFree + impossibleSafetyMargin,
		RunMaintenance:     false,
		MaintenanceMode:    "quick",
		Apply:              true,
	})
	if err != nil {
		t.Fatalf("RunPreflight() error = %v", err)
	}

	if code != app.ExitInsufficientSpace {
		t.Fatalf("RunPreflight() code = %d, want %d", code, app.ExitInsufficientSpace)
	}

	if len(result.DeletedIDs) == 0 {
		t.Fatal("expected preflight to delete at least one snapshot")
	}

	afterSnapshots, err := env.client.ListSnapshots(ctx, env.sourceDir)
	if err != nil {
		t.Fatalf("ListSnapshots() after preflight error = %v", err)
	}

	if len(afterSnapshots) >= len(env.before) {
		t.Fatalf("expected fewer snapshots after preflight, before=%d after=%d", len(env.before), len(afterSnapshots))
	}
	if result.ReadyForBackup {
		t.Fatal("expected backup to remain blocked in integration test")
	}
}

func TestPreflightIntegrationStopsAtRetentionFloorWhenStillInsufficient(t *testing.T) {
	t.Parallel()

	ctx, cancel := integrationContext(t)
	defer cancel()

	env := setupIntegrationEnv(t, ctx, 5)

	result, code, err := app.RunPreflight(ctx, os.Stdout, app.Config{
		Source:             env.sourceDir,
		SpacePath:          env.repoDir,
		KopiaBinary:        "kopia",
		KopiaConfigFile:    env.configFile,
		KopiaPassword:      env.password,
		MaxPasses:          10,
		BatchSize:          10,
		KeepLatest:         1,
		EstimateMultiplier: 1,
		ServiceReserve:     0,
		SafetyMargin:       env.beforeFree + impossibleSafetyMargin,
		RunMaintenance:     false,
		MaintenanceMode:    "quick",
		Apply:              true,
	})
	if err != nil {
		t.Fatalf("RunPreflight() error = %v", err)
	}

	if code != app.ExitInsufficientSpace {
		t.Fatalf("RunPreflight() code = %d, want %d", code, app.ExitInsufficientSpace)
	}
	if result.ReadyForBackup {
		t.Fatal("expected backup to remain blocked when even all candidates are exhausted")
	}

	afterSnapshots, err := env.client.ListSnapshots(ctx, env.sourceDir)
	if err != nil {
		t.Fatalf("ListSnapshots() after preflight error = %v", err)
	}

	keptIDs := snapshotIDs(result.Plan.Kept)
	afterIDs := snapshotIDsFromSnapshots(afterSnapshots)

	if len(afterSnapshots) != len(result.Plan.Kept) {
		t.Fatalf("expected preflight to stop at retention floor, kept=%d after=%d", len(result.Plan.Kept), len(afterSnapshots))
	}
	if len(result.DeletedIDs)+len(afterSnapshots) != len(env.before) {
		t.Fatalf("expected deleted + kept to match original snapshots, before=%d deleted=%d after=%d", len(env.before), len(result.DeletedIDs), len(afterSnapshots))
	}
	for _, id := range keptIDs {
		if !slices.Contains(afterIDs, id) {
			t.Fatalf("expected kept snapshot %s to remain after preflight", id)
		}
	}
}

func integrationContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()

	if _, err := exec.LookPath("kopia"); err != nil {
		t.Skip("kopia CLI not found in PATH")
	}

	return context.WithTimeout(context.Background(), 2*time.Minute)
}

func setupIntegrationEnv(t *testing.T, ctx context.Context, snapshotCount int) integrationEnv {
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

	freeBytes, err := storage.FreeBytes(repoDir)
	if err != nil {
		t.Fatalf("FreeBytes() error = %v", err)
	}

	return integrationEnv{
		repoDir:    repoDir,
		sourceDir:  sourceDir,
		configFile: configFile,
		password:   password,
		before:     beforeSnapshots,
		beforeFree: freeBytes,
		client:     client,
	}
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

func snapshotIDs(decisions []planner.Decision) []string {
	ids := make([]string, 0, len(decisions))
	for _, decision := range decisions {
		ids = append(ids, decision.Snapshot.ID)
	}
	return ids
}

func snapshotIDsFromSnapshots(snapshots []kopia.Snapshot) []string {
	ids := make([]string, 0, len(snapshots))
	for _, snapshot := range snapshots {
		ids = append(ids, snapshot.ID)
	}
	return ids
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
