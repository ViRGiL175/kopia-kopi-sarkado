package main

import (
	"context"
	"fmt"
	mathrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/app"
	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/kopia"
	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/storage"
)

const impossibleSafetyMargin = int64(1 << 30)
const heavySnapshotCount = 4
const heavyFileCount = 4
const heavyFileSize = int64(8 << 20)
const heavyEstimateFloor = int64(16 << 20)

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

func TestPreflightIntegrationSkipsDeletionWhenTargetIsHeuristicallyImpossible(t *testing.T) {
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
		t.Fatal("expected backup to remain blocked when pruning is heuristically impossible")
	}
	if len(result.DeletedIDs) != 0 {
		t.Fatalf("expected no deletions for heuristically impossible target, got %d", len(result.DeletedIDs))
	}
	if result.PassesExecuted != 0 {
		t.Fatalf("expected zero passes executed for heuristically impossible target, got %d", result.PassesExecuted)
	}
	if result.FreeBytes+result.MaxReclaimable >= result.RequiredBytes {
		t.Fatalf("expected optimistic reclaim to stay below required headroom, free=%d reclaim=%d required=%d", result.FreeBytes, result.MaxReclaimable, result.RequiredBytes)
	}

	afterSnapshots, err := env.client.ListSnapshots(ctx, env.sourceDir)
	if err != nil {
		t.Fatalf("ListSnapshots() after preflight error = %v", err)
	}
	if len(afterSnapshots) != len(env.before) {
		t.Fatalf("expected snapshot count to stay unchanged, before=%d after=%d", len(env.before), len(afterSnapshots))
	}
}
func TestPreflightIntegrationLargeDataset(t *testing.T) {
	ctx, cancel := integrationContextWithTimeout(t, 10*time.Minute)
	defer cancel()

	env := setupLargeIntegrationEnv(t, ctx, heavySnapshotCount, heavyFileCount, heavyFileSize)

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
		t.Fatal("expected backup to be allowed for heavy integration dataset")
	}
	if len(result.DeletedIDs) != 0 {
		t.Fatalf("expected no deletions in heavy ready path, got %d", len(result.DeletedIDs))
	}
	if result.EstimateBytes < heavyEstimateFloor {
		t.Fatalf("expected estimate >= %d bytes for heavy dataset, got %d", heavyEstimateFloor, result.EstimateBytes)
	}

	afterSnapshots, err := env.client.ListSnapshots(ctx, env.sourceDir)
	if err != nil {
		t.Fatalf("ListSnapshots() after preflight error = %v", err)
	}
	if len(afterSnapshots) != len(env.before) {
		t.Fatalf("expected snapshot count to stay unchanged, before=%d after=%d", len(env.before), len(afterSnapshots))
	}
}

func integrationContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return integrationContextWithTimeout(t, 2*time.Minute)
}

func integrationContextWithTimeout(t *testing.T, timeout time.Duration) (context.Context, context.CancelFunc) {
	t.Helper()

	if _, err := exec.LookPath("kopia"); err != nil {
		t.Skip("kopia CLI not found in PATH")
	}

	return context.WithTimeout(context.Background(), timeout)
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

func setupLargeIntegrationEnv(t *testing.T, ctx context.Context, snapshotCount, fileCount int, fileSize int64) integrationEnv {
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
		writeLargeDatasetIteration(t, sourceDir, i, fileCount, fileSize)
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

func writeLargeDatasetIteration(t *testing.T, sourceDir string, iteration, fileCount int, fileSize int64) {
	t.Helper()

	for fileIndex := 0; fileIndex < fileCount; fileIndex++ {
		path := filepath.Join(sourceDir, fmt.Sprintf("blob-%02d.bin", fileIndex))
		if iteration == 0 || fileIndex == iteration%fileCount {
			writePseudoRandomFile(t, path, fileSize, int64(iteration*fileCount+fileIndex+1))
		}
	}

	manifest := fmt.Sprintf("iteration=%d\nupdated=blob-%02d.bin\n", iteration, iteration%fileCount)
	mustWriteFile(t, filepath.Join(sourceDir, "manifest.txt"), []byte(manifest))
}

func writePseudoRandomFile(t *testing.T, path string, size int64, seed int64) {
	t.Helper()

	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create(%q) error = %v", path, err)
	}
	defer file.Close()

	rng := mathrand.New(mathrand.NewSource(seed))
	buffer := make([]byte, 1<<20)
	var written int64

	for written < size {
		chunk := len(buffer)
		remaining := size - written
		if remaining < int64(chunk) {
			chunk = int(remaining)
		}

		if _, err := rng.Read(buffer[:chunk]); err != nil {
			t.Fatalf("rng.Read() error = %v", err)
		}
		if _, err := file.Write(buffer[:chunk]); err != nil {
			t.Fatalf("Write(%q) error = %v", path, err)
		}

		written += int64(chunk)
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
