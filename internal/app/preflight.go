package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/kopia"
	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/planner"
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
	KopiaBinary        string
	KopiaConfigFile    string
	KopiaEnv           []string
	RepositoryID       string
	OperationFile      string
	MaxPasses          int
	BatchSize          int
	KeepLatest         int
	EstimateMultiplier float64
	ServiceReserve     int64
	SafetyMargin       int64
	RunMaintenance     bool
	MaintenanceMode    string
	Apply              bool
	ExclusiveAccess    bool
	ProtectTags        []string
}

type Result struct {
	RepositoryID   string
	Source         kopia.Source
	EstimateBytes  int64
	FreeBytes      int64
	CapacityBytes  int64
	RequiredBytes  int64
	Plan           planner.Result
	DeletedIDs     []string
	UncertainIDs   []string
	PassesExecuted int
	ReadyForBackup bool
}

func (c Config) Validate() error {
	if !filepath.IsAbs(c.Source) || !filepath.IsAbs(c.KopiaConfigFile) {
		return errors.New("absolute source and config-file paths are required")
	}
	if c.MaxPasses < 1 || c.MaxPasses > 100 || c.BatchSize < 1 || c.BatchSize > 100 || c.KeepLatest < 1 {
		return errors.New("max-passes and batch-size must be 1..100; keep-latest must be positive")
	}
	if c.EstimateMultiplier < 1 || math.IsNaN(c.EstimateMultiplier) || math.IsInf(c.EstimateMultiplier, 0) || c.ServiceReserve < 0 || c.SafetyMargin < 0 {
		return errors.New("invalid headroom parameters")
	}
	if c.MaintenanceMode != "quick" && c.MaintenanceMode != "full" {
		return errors.New("maintenance-mode must be quick or full")
	}
	for _, tag := range c.ProtectTags {
		key, _, ok := strings.Cut(tag, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return errors.New("protect-tag must have a nonempty key and '='")
		}
	}
	if c.Apply && (c.RepositoryID == "" || !filepath.IsAbs(c.OperationFile)) {
		return errors.New("apply requires repository-id and an absolute operation-file")
	}
	if c.Apply && !c.ExclusiveAccess {
		return errors.New("apply requires --exclusive-access: externally serialize all repository writers and policy changes")
	}
	return nil
}

func RunPreflight(ctx context.Context, stdout io.Writer, cfg Config) (result Result, code int, err error) {
	if err = cfg.Validate(); err != nil {
		return result, ExitUsage, err
	}
	cfg.Source = filepath.Clean(cfg.Source)
	client, err := (kopia.Client{Binary: cfg.KopiaBinary, ConfigFile: cfg.KopiaConfigFile, Env: cfg.KopiaEnv}).Resolve()
	if err != nil {
		return result, ExitRuntime, err
	}
	if err = client.CheckVersion(ctx); err != nil {
		return result, ExitRuntime, err
	}
	status, err := client.Status(ctx)
	if err != nil {
		return result, ExitRuntime, err
	}
	if cfg.RepositoryID != "" && status.ID != cfg.RepositoryID {
		return result, ExitRuntime, errors.New("repository ID mismatch")
	}
	if cfg.Apply && status.ClientOptions.ReadOnly {
		return result, ExitRuntime, errors.New("repository connection is read-only")
	}
	if cfg.Apply {
		if err = validateJournalLocation(cfg.OperationFile, cfg.Source, status.Storage.Config.Path); err != nil {
			return result, ExitUsage, err
		}
		if _, statErr := os.Lstat(cfg.OperationFile); !os.IsNotExist(statErr) {
			return result, ExitRuntime, errors.New("operation-file exists or cannot be checked; inspect the prior operation before allowing another backup or deletion")
		}
	}
	source := status.Source(cfg.Source)
	estimateBytes, err := client.EstimateSnapshotSize(ctx, cfg.Source)
	if err != nil {
		return result, ExitRuntime, err
	}
	requiredBytes, err := units.RequiredBytes(estimateBytes, cfg.EstimateMultiplier, cfg.ServiceReserve, cfg.SafetyMargin)
	if err != nil {
		return result, ExitUsage, err
	}
	snapshots, err := client.ListSnapshots(ctx, source)
	if err != nil {
		return result, ExitRuntime, err
	}
	options := planner.Options{KeepLatest: cfg.KeepLatest, ProtectTags: cfg.ProtectTags}
	plan := planner.BuildPlan(snapshots, options)
	statusNow, err := client.Status(ctx)
	if err != nil {
		return result, ExitRuntime, err
	}
	if !sameRepository(status, statusNow) {
		return result, ExitRuntime, errors.New("repository connection changed during analysis")
	}
	result = Result{RepositoryID: status.ID, Source: source, EstimateBytes: estimateBytes, FreeBytes: statusNow.FreeBytes(), CapacityBytes: statusNow.CapacityBytes(), RequiredBytes: requiredBytes, Plan: plan}
	if err = reportPlan(stdout, result, cfg); err != nil {
		return result, ExitRuntime, err
	}
	if result.FreeBytes >= requiredBytes {
		result.ReadyForBackup = true
		return result, ExitOK, nil
	}
	if requiredBytes > result.CapacityBytes {
		return result, ExitInsufficientSpace, nil
	}
	if !cfg.Apply || len(plan.Candidates) == 0 {
		return result, ExitInsufficientSpace, nil
	}
	if cfg.RunMaintenance {
		if err = client.CheckMaintenanceOwner(ctx, status); err != nil {
			return result, ExitRuntime, err
		}
	}
	journal, err := createJournal(cfg.OperationFile, result, cfg)
	if err != nil {
		return result, ExitRuntime, err
	}
	defer func() {
		finishErr := journal.record(journalEvent{Event: "finished", Deleted: result.DeletedIDs, Uncertain: result.UncertainIDs, ExitCode: &code})
		closeErr := journal.Close()
		if finishErr != nil || closeErr != nil {
			code = ExitRuntime
			result.ReadyForBackup = false
			err = errors.Join(err, errors.New("operation journal could not be finalized; inspect repository before retrying"))
		}
	}()
	if _, err = fmt.Fprintln(stdout, "operation: started"); err != nil {
		return result, ExitRuntime, err
	}
	remaining := append([]planner.Decision(nil), plan.Candidates...)
	expected := snapshots
	for pass := 1; pass <= cfg.MaxPasses && len(remaining) > 0; pass++ {
		beforeFree := result.FreeBytes
		count := min(cfg.BatchSize, len(remaining))
		currentBatch := remaining[:count]
		remaining = remaining[count:]
		for _, decision := range currentBatch {
			currentStatus, current, checkErr := refresh(ctx, client, status, source, expected)
			if checkErr != nil {
				return result, ExitRuntime, checkErr
			}
			result.FreeBytes = currentStatus.FreeBytes()
			if requiredBytes > currentStatus.CapacityBytes() {
				return result, ExitInsufficientSpace, nil
			}
			if result.FreeBytes >= requiredBytes {
				result.ReadyForBackup = true
				return result, ExitOK, nil
			}
			candidates := planner.BuildPlan(current, options).Candidates
			if !slices.ContainsFunc(candidates, func(d planner.Decision) bool { return d.Snapshot.ID == decision.Snapshot.ID }) {
				return result, ExitRuntime, errors.New("candidate became protected; stop and inspect the operation")
			}
			if err = journal.record(journalEvent{Event: "delete-intent", ID: decision.Snapshot.ID}); err != nil {
				return result, ExitRuntime, err
			}
			deleteErr := client.DeleteSnapshot(ctx, decision.Snapshot.ID)
			if deleteErr == nil {
				if _, err = fmt.Fprintf(stdout, "operation: delete-command-completed %s\n", decision.Snapshot.ID); err != nil {
					result.UncertainIDs = append(result.UncertainIDs, decision.Snapshot.ID)
					_ = journal.record(journalEvent{Event: "delete-unknown", ID: decision.Snapshot.ID})
					return result, ExitRuntime, errors.New("cannot report completed delete command; inspect journal and repository before retrying")
				}
			}
			afterStatus, readErr := client.Status(ctx)
			var after []kopia.Snapshot
			if readErr == nil && !sameRepository(status, afterStatus) {
				readErr = errors.New("repository connection changed")
			}
			if readErr == nil {
				after, readErr = client.ListSnapshots(ctx, source)
			}
			if readErr != nil {
				result.UncertainIDs = append(result.UncertainIDs, decision.Snapshot.ID)
				_ = journal.record(journalEvent{Event: "delete-unknown", ID: decision.Snapshot.ID})
				return result, ExitRuntime, errors.Join(deleteErr, readErr, errors.New("deletion outcome is unknown; do not start a new operation"))
			}
			present := slices.ContainsFunc(after, func(s kopia.Snapshot) bool { return s.ID == decision.Snapshot.ID })
			event := "delete-present"
			if !present {
				result.DeletedIDs = append(result.DeletedIDs, decision.Snapshot.ID)
				expected = slices.DeleteFunc(expected, func(s kopia.Snapshot) bool { return s.ID == decision.Snapshot.ID })
				event = "delete-confirmed"
			}
			if err = journal.record(journalEvent{Event: event, ID: decision.Snapshot.ID}); err != nil {
				return result, ExitRuntime, err
			}
			if deleteErr != nil {
				return result, ExitRuntime, deleteErr
			}
			if present {
				return result, ExitRuntime, errors.New("Kopia returned success but the snapshot still exists")
			}
			if !sameInventory(expected, after) {
				return result, ExitRuntime, errors.New("snapshot inventory changed outside this operation")
			}
			result.FreeBytes = afterStatus.FreeBytes()
			if result.FreeBytes >= requiredBytes {
				result.ReadyForBackup = true
				return result, ExitOK, nil
			}
		}
		result.PassesExecuted = pass
		if cfg.RunMaintenance {
			currentStatus, _, checkErr := refresh(ctx, client, status, source, expected)
			if checkErr != nil {
				return result, ExitRuntime, checkErr
			}
			if err = client.CheckMaintenanceOwner(ctx, currentStatus); err != nil {
				return result, ExitRuntime, err
			}
			if err = journal.record(journalEvent{Event: "maintenance-intent", Mode: cfg.MaintenanceMode}); err != nil {
				return result, ExitRuntime, err
			}
			maintenanceErr := client.RunMaintenance(ctx, cfg.MaintenanceMode)
			event := "maintenance-confirmed"
			if maintenanceErr != nil {
				event = "maintenance-unknown"
			}
			if err = journal.record(journalEvent{Event: event, Mode: cfg.MaintenanceMode}); err != nil {
				return result, ExitRuntime, err
			}
			if maintenanceErr != nil {
				return result, ExitRuntime, maintenanceErr
			}
		}
		currentStatus, _, checkErr := refresh(ctx, client, status, source, expected)
		if checkErr != nil {
			return result, ExitRuntime, checkErr
		}
		result.FreeBytes = currentStatus.FreeBytes()
		if result.FreeBytes >= requiredBytes {
			result.ReadyForBackup = true
			return result, ExitOK, nil
		}
		if result.FreeBytes <= beforeFree {
			return result, ExitInsufficientSpace, nil
		}
	}
	return result, ExitInsufficientSpace, nil
}

func sameRepository(a, b kopia.RepositoryStatus) bool {
	return a.ID == b.ID && a.ConfigFile == b.ConfigFile && a.Storage == b.Storage && a.ClientOptions == b.ClientOptions
}

func refresh(ctx context.Context, client kopia.Client, original kopia.RepositoryStatus, source kopia.Source, expected []kopia.Snapshot) (kopia.RepositoryStatus, []kopia.Snapshot, error) {
	status, err := client.Status(ctx)
	if err != nil {
		return status, nil, err
	}
	if !sameRepository(original, status) {
		return status, nil, errors.New("repository connection changed")
	}
	current, err := client.ListSnapshots(ctx, source)
	if err != nil {
		return status, nil, err
	}
	if !sameInventory(expected, current) {
		return status, nil, errors.New("snapshot inventory changed outside this operation")
	}
	return status, current, nil
}

func sameInventory(a, b []kopia.Snapshot) bool {
	normalized := func(items []kopia.Snapshot) string {
		copyItems := append([]kopia.Snapshot{}, items...)
		sort.Slice(copyItems, func(i, j int) bool { return copyItems[i].ID < copyItems[j].ID })
		for i := range copyItems {
			copyItems[i].RetentionReasons = nil
		}
		data, _ := json.Marshal(copyItems)
		return string(data)
	}
	return normalized(a) == normalized(b)
}

func reportPlan(w io.Writer, result Result, cfg Config) error {
	var report strings.Builder
	fmt.Fprintf(&report, "repository-id: %s\nsource: %q@%q:%q\n", result.RepositoryID, result.Source.UserName, result.Source.Host, result.Source.Path)
	fmt.Fprintf(&report, "estimate (heuristic): %s\nfree: %s\nrequired-headroom: %s\n", units.FormatBytes(result.EstimateBytes), units.FormatBytes(result.FreeBytes), units.FormatBytes(result.RequiredBytes))
	fmt.Fprintf(&report, "storage-capacity: %s\n", units.FormatBytes(result.CapacityBytes))
	fmt.Fprintf(&report, "apply: %t\ndelete-budget: %d\nmaintenance: %t (%s, repository-wide)\n", cfg.Apply, cfg.MaxPasses*cfg.BatchSize, cfg.RunMaintenance, cfg.MaintenanceMode)
	fmt.Fprintln(&report, "reclaimable-space: unknown; snapshot deletion does not immediately reclaim content")
	for _, decision := range result.Plan.Kept {
		fmt.Fprintf(&report, "  keep %s: %s\n", decision.Snapshot.ID, decision.Reason)
	}
	for _, decision := range result.Plan.Candidates {
		fmt.Fprintf(&report, "  candidate %s: %s\n", decision.Snapshot.ID, decision.Reason)
	}
	_, err := io.WriteString(w, report.String())
	return err
}
