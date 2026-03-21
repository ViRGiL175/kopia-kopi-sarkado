package app

import (
	"context"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/kopia"
	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/planner"
	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/storage"
	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/units"
)

const (
	ExitOK                = 0
	ExitInsufficientSpace = 2
	ExitUsage             = 3
	ExitRuntime           = 4
)

type Config struct {
	Source             string
	SpacePath          string
	KopiaBinary        string
	KopiaConfigFile    string
	KopiaPassword      string
	MaxPasses          int
	BatchSize          int
	KeepLatest         int
	EstimateMultiplier float64
	ServiceReserve     int64
	SafetyMargin       int64
	RunMaintenance     bool
	MaintenanceMode    string
	Apply              bool
	ProtectTags        []string
}

type Result struct {
	EstimateBytes  int64
	FreeBytes      int64
	RequiredBytes  int64
	MaxReclaimable int64
	Plan           planner.Result
	DeletedIDs     []string
	PassesExecuted int
	ReadyForBackup bool
}

type preflightClient interface {
	EstimateSnapshotSize(ctx context.Context, source string) (int64, error)
	ListSnapshots(ctx context.Context, source string) ([]kopia.Snapshot, error)
	DeleteSnapshot(ctx context.Context, snapshotID string) error
	RunMaintenance(ctx context.Context, mode string) error
}

type freeBytesFunc func(path string) (int64, error)

func RunPreflight(ctx context.Context, stdout io.Writer, cfg Config) (Result, int, error) {
	client := kopia.Client{Binary: cfg.KopiaBinary, ConfigFile: cfg.KopiaConfigFile, Password: cfg.KopiaPassword}
	return runPreflight(ctx, stdout, cfg, client, storage.FreeBytes)
}

func runPreflight(ctx context.Context, stdout io.Writer, cfg Config, client preflightClient, freeBytesProbe freeBytesFunc) (Result, int, error) {
	if cfg.MaxPasses < 1 {
		cfg.MaxPasses = 1
	}
	if cfg.BatchSize < 1 {
		cfg.BatchSize = 1
	}
	if cfg.KeepLatest < 1 {
		cfg.KeepLatest = 1
	}
	if cfg.EstimateMultiplier < 1 {
		cfg.EstimateMultiplier = 1
	}
	if strings.TrimSpace(cfg.MaintenanceMode) == "" {
		cfg.MaintenanceMode = "quick"
	}

	estimateBytes, err := client.EstimateSnapshotSize(ctx, cfg.Source)
	if err != nil {
		return Result{}, ExitRuntime, err
	}

	freeBytes, err := freeBytesProbe(cfg.SpacePath)
	if err != nil {
		return Result{}, ExitRuntime, err
	}

	requiredBytes := int64(math.Ceil(float64(estimateBytes)*cfg.EstimateMultiplier)) + cfg.ServiceReserve + cfg.SafetyMargin

	snapshots, err := client.ListSnapshots(ctx, cfg.Source)
	if err != nil {
		return Result{}, ExitRuntime, err
	}

	plan := planner.BuildPlan(snapshots, planner.Options{
		KeepLatest:  cfg.KeepLatest,
		ProtectTags: cfg.ProtectTags,
	})

	result := Result{
		EstimateBytes: estimateBytes,
		FreeBytes:     freeBytes,
		RequiredBytes: requiredBytes,
		MaxReclaimable: optimisticMaxReclaimableBytes(estimateBytes, len(plan.Candidates)),
		Plan:          plan,
	}

	reportPlan(stdout, result, cfg)

	if freeBytes >= requiredBytes {
		result.ReadyForBackup = true
		fmt.Fprintln(stdout, "status: ready")
		return result, ExitOK, nil
	}

	if freeBytes+result.MaxReclaimable < requiredBytes {
		fmt.Fprintln(stdout, "status: insufficient-space (heuristically impossible even after pruning all candidates)")
		return result, ExitInsufficientSpace, nil
	}

	if !cfg.Apply {
		fmt.Fprintln(stdout, "status: insufficient-space (dry-run)")
		return result, ExitInsufficientSpace, nil
	}

	remaining := append([]planner.Decision(nil), plan.Candidates...)
	for pass := 1; pass <= cfg.MaxPasses && len(remaining) > 0; pass++ {
		count := cfg.BatchSize
		if count > len(remaining) {
			count = len(remaining)
		}

		currentBatch := remaining[:count]
		remaining = remaining[count:]

		fmt.Fprintf(stdout, "pass %d: deleting %d snapshot(s)\n", pass, len(currentBatch))
		for _, decision := range currentBatch {
			fmt.Fprintf(stdout, "  delete %s (%s)\n", decision.Snapshot.ID, decision.Reason)
			if err := client.DeleteSnapshot(ctx, decision.Snapshot.ID); err != nil {
				return result, ExitRuntime, err
			}

			result.DeletedIDs = append(result.DeletedIDs, decision.Snapshot.ID)
		}

		if cfg.RunMaintenance {
			fmt.Fprintf(stdout, "pass %d: running maintenance (%s)\n", pass, cfg.MaintenanceMode)
			if err := client.RunMaintenance(ctx, cfg.MaintenanceMode); err != nil {
				return result, ExitRuntime, err
			}
		}

		freeBytes, err = freeBytesProbe(cfg.SpacePath)
		if err != nil {
			return result, ExitRuntime, err
		}

		result.FreeBytes = freeBytes
		result.PassesExecuted = pass
		fmt.Fprintf(stdout, "pass %d: free=%s required=%s\n", pass, units.FormatBytes(freeBytes), units.FormatBytes(requiredBytes))

		if freeBytes >= requiredBytes {
			result.ReadyForBackup = true
			fmt.Fprintln(stdout, "status: ready")
			return result, ExitOK, nil
		}
	}

	fmt.Fprintln(stdout, "status: insufficient-space")
	return result, ExitInsufficientSpace, nil
}

func reportPlan(stdout io.Writer, result Result, cfg Config) {
	fmt.Fprintf(stdout, "estimate: %s\n", units.FormatBytes(result.EstimateBytes))
	fmt.Fprintf(stdout, "free: %s\n", units.FormatBytes(result.FreeBytes))
	fmt.Fprintf(stdout, "required-headroom: %s\n", units.FormatBytes(result.RequiredBytes))
	fmt.Fprintf(stdout, "max-reclaimable-headroom: %s\n", units.FormatBytes(result.MaxReclaimable))
	fmt.Fprintf(stdout, "apply: %t\n", cfg.Apply)
	fmt.Fprintf(stdout, "planned-candidates: %d\n", len(result.Plan.Candidates))

	limit := 10
	if len(result.Plan.Candidates) < limit {
		limit = len(result.Plan.Candidates)
	}
	for i := 0; i < limit; i++ {
		decision := result.Plan.Candidates[i]
		fmt.Fprintf(stdout, "  candidate %d: %s %s\n", i+1, decision.Snapshot.ID, decision.Snapshot.EndTime.Format("2006-01-02T15:04:05Z07:00"))
	}
}

func optimisticMaxReclaimableBytes(estimateBytes int64, candidateCount int) int64 {
	if estimateBytes <= 0 || candidateCount <= 0 {
		return 0
	}

	const maxInt64 = int64(1<<63 - 1)
	if int64(candidateCount) > maxInt64/estimateBytes {
		return maxInt64
	}

	return estimateBytes * int64(candidateCount)
}
