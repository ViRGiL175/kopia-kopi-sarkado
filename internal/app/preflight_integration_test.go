package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/app"
	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/testrepo"
)

func prepared(t *testing.T) (*testrepo.Repo, app.Config) {
	t.Helper()
	r := testrepo.New(t)
	for i := 0; i < 10; i++ {
		if i == 4 {
			r.Snapshot(i, "--pin=important")
		} else if i == 5 {
			r.Snapshot(i, "--tags=important:yes")
		} else {
			r.Snapshot(i)
		}
	}
	r.Retention(r.Source, 2)
	cfg := r.Config()
	cfg.Apply = true
	cfg.SafetyMargin = r.InsufficientMargin()
	cfg.ProtectTags = []string{"tag:important=yes"}
	return r, cfg
}

func TestRunPreflightBoundsDeletionAndRestoresProtectedVersions(t *testing.T) {
	for _, maintenance := range []bool{false, true} {
		t.Run(fmt.Sprintf("maintenance-%t", maintenance), func(t *testing.T) {
			r, cfg := prepared(t)
			cfg.RunMaintenance = maintenance
			before := r.Snapshots()
			infoBefore := r.MaintenanceInfo()
			result, code, err := app.RunPreflight(r.Context, io.Discard, cfg)
			if err != nil || code != app.ExitInsufficientSpace || len(result.DeletedIDs) != 2 {
				t.Fatalf("code=%d deleted=%v error=%v", code, result.DeletedIDs, err)
			}
			after := r.Snapshots()
			if len(after) != len(before)-2 {
				t.Fatal("unexpected number of deleted snapshots")
			}
			for _, d := range result.Plan.Kept {
				var found bool
				for _, s := range after {
					if s.ID == d.Snapshot.ID {
						found = true
					}
				}
				if !found {
					t.Fatal("protected snapshot lost")
				}
			}
			for _, s := range after {
				r.Restore(s, fmt.Sprintf("synthetic version %d\n", s.EndTime.Minute()))
			}
			if !maintenance {
				var a, b any
				_ = json.Unmarshal(infoBefore, &a)
				_ = json.Unmarshal(r.MaintenanceInfo(), &b)
				if !reflect.DeepEqual(a, b) {
					t.Fatal("automatic maintenance ran without authorization")
				}
			}
			_, code, err = app.RunPreflight(r.Context, io.Discard, cfg)
			if code != app.ExitRuntime || err == nil || len(r.Snapshots()) != len(after) {
				t.Fatal("same operation restarted deletion budget")
			}
		})
	}
}

func TestRunPreflightRejectsWrongRepositoryAndJournalFailure(t *testing.T) {
	r, cfg := prepared(t)
	before := len(r.Snapshots())
	cfg.RepositoryID = "wrong"
	if _, code, err := app.RunPreflight(r.Context, io.Discard, cfg); code != app.ExitRuntime || err == nil {
		t.Fatal("wrong repository was accepted")
	}
	cfg.RepositoryID = r.Status.ID
	cfg.OperationFile = filepath.Join(r.Root, "missing-directory", "operation")
	if _, _, err := app.RunPreflight(r.Context, io.Discard, cfg); err == nil {
		t.Fatal("missing journal directory was accepted")
	}
	cfg.OperationFile = filepath.Join(r.Storage, "journal")
	if _, _, err := app.RunPreflight(r.Context, io.Discard, cfg); err == nil {
		t.Fatal("journal inside storage was accepted")
	}
	if len(r.Snapshots()) != before {
		t.Fatal("validation error deleted snapshots")
	}
}

type onPlan func()

func (f onPlan) Write(p []byte) (int, error) {
	if bytes.HasPrefix(p, []byte("repository-id:")) {
		f()
	}
	return len(p), nil
}

func TestRunPreflightRejectsConcurrentPinChange(t *testing.T) {
	r, cfg := prepared(t)
	before := r.Snapshots()
	writer := onPlan(func() { r.Run("snapshot", "pin", "--add=concurrent", before[2].ID) })
	result, code, err := app.RunPreflight(r.Context, writer, cfg)
	if err == nil || code != app.ExitRuntime || len(result.DeletedIDs) != 0 {
		t.Fatalf("concurrent pin not detected: %d %v", code, err)
	}
	if len(r.Snapshots()) != len(before) {
		t.Fatal("concurrent update caused deletion")
	}
}

func TestRunPreflightCancellationRecordsPartialOutcomeAndBlocksRetry(t *testing.T) {
	r, cfg := prepared(t)
	cfg.BatchSize = 4
	ctx, cancel := context.WithCancel(r.Context)
	defer cancel()
	watcherDone := make(chan bool, 1)
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				watcherDone <- false
				return
			case <-ticker.C:
				b, err := os.ReadFile(cfg.OperationFile)
				if err == nil && bytes.Contains(b, []byte("delete-confirmed")) {
					cancel()
					watcherDone <- true
					return
				}
			}
		}
	}()
	result, code, err := app.RunPreflight(ctx, io.Discard, cfg)
	cancel()
	if !<-watcherDone {
		t.Fatal("did not observe a real completed deletion to interrupt after")
	}
	if err == nil || code != app.ExitRuntime || len(result.DeletedIDs) == 0 {
		t.Fatalf("partial outcome missing: code=%d deleted=%v error=%v", code, result.DeletedIDs, err)
	}
	journal, readErr := os.ReadFile(cfg.OperationFile)
	if readErr != nil || !strings.Contains(string(journal), "delete-intent") {
		t.Fatal("missing durable delete intent")
	}
	after := r.Snapshots()
	for _, s := range after {
		r.Restore(s, fmt.Sprintf("synthetic version %d\n", s.EndTime.Minute()))
	}
	_, code, err = app.RunPreflight(r.Context, io.Discard, cfg)
	if code != app.ExitRuntime || err == nil || len(r.Snapshots()) != len(after) {
		t.Fatal("interrupted operation restarted")
	}
}

func TestRunPreflightStopsAtNativeRetentionFloor(t *testing.T) {
	r, cfg := prepared(t)
	r.Retention(r.Source, 100)
	result, code, err := app.RunPreflight(r.Context, io.Discard, cfg)
	if err != nil || code != app.ExitInsufficientSpace || len(result.DeletedIDs) != 0 || len(result.Plan.Candidates) != 0 {
		t.Fatalf("native floor not respected: code=%d error=%v", code, err)
	}
	for _, s := range r.Snapshots() {
		if !s.Healthy() {
			t.Fatalf("unexpected incomplete fixture: %+v", s)
		}
	}
}
