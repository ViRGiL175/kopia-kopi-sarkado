package main

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/app"
	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/planner"
	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/testrepo"
)

func TestPreflightIntegrationReadyWithoutDeletion(t *testing.T) {
	r := testrepo.New(t)
	for i := 0; i < 3; i++ {
		r.Snapshot(i)
	}
	before := r.Snapshots()
	cfg := r.Config()
	cfg.Apply = true
	result, code, err := app.RunPreflight(r.Context, io.Discard, cfg)
	if err != nil || code != app.ExitOK || !result.ReadyForBackup || len(result.DeletedIDs) != 0 {
		t.Fatalf("result=%+v code=%d error=%v", result, code, err)
	}
	if len(r.Snapshots()) != len(before) {
		t.Fatal("ready preflight changed snapshots")
	}
	if _, err := os.Stat(cfg.OperationFile); !os.IsNotExist(err) {
		t.Fatal("ready preflight must not start a deletion operation")
	}
}

func TestPreflightIntegrationInsufficientDryRun(t *testing.T) {
	r := testrepo.New(t)
	for i := 0; i < 8; i++ {
		r.Snapshot(i)
	}
	r.Retention(r.Source, 1)
	cfg := r.Config()
	cfg.SafetyMargin = 1 << 50
	var report bytes.Buffer
	result, code, err := app.RunPreflight(r.Context, &report, cfg)
	if err != nil || code != app.ExitInsufficientSpace || result.ReadyForBackup || len(result.DeletedIDs) != 0 {
		t.Fatalf("code=%d error=%v result=%+v", code, err, result)
	}
	if len(result.Plan.Candidates) == 0 {
		t.Fatal("fixture must have disposable snapshots")
	}
	for _, d := range result.Plan.Candidates {
		if !bytes.Contains(report.Bytes(), []byte(d.Snapshot.ID)) {
			t.Fatal("preview omitted a candidate")
		}
	}
	if len(r.Snapshots()) != 8 {
		t.Fatal("dry run changed snapshots")
	}
	cfg.Apply = true
	result, code, err = app.RunPreflight(r.Context, io.Discard, cfg)
	if err != nil || code != app.ExitInsufficientSpace || len(result.DeletedIDs) != 0 || len(r.Snapshots()) != 8 {
		t.Fatal("physically impossible headroom target must not delete snapshots")
	}
}

func TestRealKopiaRetentionPinsAndNestedSources(t *testing.T) {
	r := testrepo.New(t)
	for i := 0; i < 10; i++ {
		if i == 3 {
			r.Snapshot(i, "--pin=keep-forever")
		} else {
			r.Snapshot(i)
		}
	}
	parent := filepath.Dir(r.Source)
	r.Run("snapshot", "create", parent, "--force-disable-actions", "--no-send-snapshot-report")
	r.Retention(r.Source, 2)
	history := r.Snapshots()
	if len(history) != 10 {
		t.Fatalf("exact source must exclude parent: got %d", len(history))
	}
	plan := planner.BuildPlan(history, planner.Options{KeepLatest: 1})
	var pins, retained int
	for _, s := range history {
		if s.Source != r.Status.Source(r.Source) {
			t.Fatal("ancestor leaked into exact source")
		}
		if !s.Healthy() {
			t.Fatalf("complete real snapshot classified unhealthy: %+v", s)
		}
		if len(s.Pins) > 0 {
			pins++
		}
		if len(s.RetentionReasons) > 0 {
			retained++
		}
		for _, d := range plan.Candidates {
			if s.ID == d.Snapshot.ID && (len(s.Pins) > 0 || len(s.RetentionReasons) > 0) {
				t.Fatal("native protection lost")
			}
		}
	}
	if pins != 1 || retained < 2 || len(plan.Candidates) == 0 {
		t.Fatalf("fixture not exercising protections: pins=%d retained=%d candidates=%d", pins, retained, len(plan.Candidates))
	}
}

func TestPreflightIntegrationWithLargeSnapshots(t *testing.T) {
	r := testrepo.New(t)
	data := make([]byte, 8<<20)
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 4; i++ {
		_, _ = rng.Read(data)
		if err := os.WriteFile(filepath.Join(r.Source, fmt.Sprintf("blob-%d", i)), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r.Snapshot(0)
	cfg := r.Config()
	result, code, err := app.RunPreflight(r.Context, io.Discard, cfg)
	if err != nil || code != app.ExitOK || result.EstimateBytes < 16<<20 {
		t.Fatalf("code=%d estimate=%d error=%v", code, result.EstimateBytes, err)
	}
}

func TestRealKopiaIncompleteSnapshotDoesNotReplaceCompleteFloor(t *testing.T) {
	r := testrepo.New(t)
	for i := 0; i < 8; i++ {
		r.Snapshot(i)
	}
	data := make([]byte, 512<<10)
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 16; i++ {
		_, _ = rng.Read(data)
		if err := os.WriteFile(filepath.Join(r.Source, fmt.Sprintf("partial-%02d", i)), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	partial := r.Snapshot(20, "--upload-limit-mb=1")
	if partial.Healthy() || partial.IncompleteReason == "" {
		t.Fatalf("real upload limit did not produce incomplete snapshot: %+v", partial)
	}
	r.Retention(r.Source, 1)
	history := r.Snapshots()
	plan := planner.BuildPlan(history, planner.Options{KeepLatest: 2})
	for _, d := range plan.Candidates {
		if d.Snapshot.ID == partial.ID || d.Snapshot.EndTime.Minute() == 6 || d.Snapshot.EndTime.Minute() == 7 {
			t.Fatal("partial snapshot replaced complete retention floor")
		}
	}
}

func TestRealKopiaRestorePathsAndLinks(t *testing.T) {
	r := testrepo.New(t)
	name := "日本語-ёж.txt"
	payload := []byte("synthetic unicode content")
	if err := os.WriteFile(filepath.Join(r.Source, name), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(r.Source, name), filepath.Join(r.Source, "hardlink")); err != nil {
		t.Fatal(err)
	}
	symlink := runtime.GOOS != "windows"
	if symlink {
		if err := os.Symlink(name, filepath.Join(r.Source, "symlink")); err != nil {
			t.Fatal(err)
		}
	}
	s := r.Snapshot(0)
	r.Restore(s, "synthetic version 0\n")
	destination := filepath.Join(r.Root, "restore-"+s.ID)
	names := []string{name, "hardlink"}
	if symlink {
		names = append(names, "symlink")
	}
	for _, n := range names {
		got, err := os.ReadFile(filepath.Join(destination, n))
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("restored %q: %v", n, err)
		}
	}
}
