//go:build linux

package app_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/app"
	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/kopia"
	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/testrepo"
)

const testImageBytes = int64(256 << 20)

func testVolume(t *testing.T) string {
	t.Helper()
	linuxSafety(t)
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" {
		t.Fatal("filesystem fault tests require a disposable GitHub-hosted runner")
	}
	volume := os.Getenv("KKS_TEST_VOLUME")
	image := os.Getenv("KKS_TEST_IMAGE")
	runnerTemp, err := filepath.EvalSymlinks(os.Getenv("RUNNER_TEMP"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{volume, image} {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || resolved != path || !filepath.IsAbs(path) || strings.ContainsAny(path, " \t\n\\") {
			t.Fatal("test image and mount must be explicit canonical private paths")
		}
		rel, err := filepath.Rel(runnerTemp, path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
			t.Fatal("test path is not inside RUNNER_TEMP")
		}
	}
	imageInfo, err := os.Stat(image)
	if err != nil || !imageInfo.Mode().IsRegular() || imageInfo.Size() != testImageBytes {
		t.Fatal("expected a regular 256MiB test image")
	}
	mountInfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	device := ""
	for _, line := range strings.Split(string(mountInfo), "\n") {
		before, after, ok := strings.Cut(line, " - ")
		a, b := strings.Fields(before), strings.Fields(after)
		if ok && len(a) > 4 && len(b) > 1 && a[4] == volume && b[0] == "ext4" {
			device = b[1]
		}
	}
	if !regexp.MustCompile(`^/dev/loop[0-9]+$`).MatchString(device) {
		t.Fatal("refusing a filesystem that is not the isolated ext4 loop mount")
	}
	backing, err := os.ReadFile("/sys/block/" + filepath.Base(device) + "/loop/backing_file")
	if err != nil || strings.TrimSpace(string(backing)) != image {
		t.Fatal("loop backing file differs from the bounded test image")
	}
	var stats syscall.Statfs_t
	if err := syscall.Statfs(volume, &stats); err != nil {
		t.Fatal(err)
	}
	capacity := int64(stats.Blocks) * int64(stats.Bsize)
	if stats.Type != 0xef53 || capacity < 128<<20 || capacity > testImageBytes {
		t.Fatal("unexpected test filesystem type or capacity")
	}
	return volume
}

func writePayload(t *testing.T, r *testrepo.Repo, version int) [32]byte {
	t.Helper()
	payload := make([]byte, 4<<20)
	_, _ = rand.New(rand.NewSource(int64(1000 + version))).Read(payload)
	if err := os.WriteFile(filepath.Join(r.Source, "payload.bin"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(payload)
}

func restorePayload(t *testing.T, r *testrepo.Repo, snapshot kopia.Snapshot, hash [32]byte) {
	t.Helper()
	r.Restore(snapshot, fmt.Sprintf("synthetic version %d\n", snapshot.EndTime.Minute()))
	payload, err := os.ReadFile(filepath.Join(r.Root, "restore-"+snapshot.ID, "payload.bin"))
	if err != nil || sha256.Sum256(payload) != hash {
		t.Fatalf("wrong retained payload for %s: %v", snapshot.ID, err)
	}
}

func fillTestVolume(t *testing.T, volume string) string {
	t.Helper()
	// Re-attest immediately before writing; the host filesystem must never be a target.
	if volume != testVolume(t) {
		t.Fatal("volume changed")
	}
	path := filepath.Join(volume, "kks-enospc-filler")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove test filler: %v", err)
		}
	})
	// fallocate reserves actual blocks synchronously. A large buffered write can
	// return ENOSPC while leaving enough space for a small Kopia metadata write.
	var offset int64
	exhausted := false
	for _, chunk := range []int64{1 << 20, 4096} {
		for offset+chunk <= testImageBytes {
			err = syscall.Fallocate(int(file.Fd()), 0, offset, chunk)
			if errors.Is(err, syscall.ENOSPC) {
				exhausted = true
				break
			}
			if err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
			offset += chunk
		}
	}
	closeErr := file.Close()
	if !exhausted || (closeErr != nil && !errors.Is(closeErr, syscall.ENOSPC)) {
		t.Fatalf("bounded filler did not reach ENOSPC: %v", closeErr)
	}
	var stats syscall.Statfs_t
	if err := syscall.Statfs(volume, &stats); err != nil || stats.Bavail != 0 {
		t.Fatalf("full-volume fixture still has available blocks: %d, error %v", stats.Bavail, err)
	}
	t.Log("confirmed ENOSPC and zero available blocks on the isolated 256MiB loop filesystem")
	return path
}

func TestLinuxFullRepositoryVolumeBlocksPruningAndPreservesCopies(t *testing.T) {
	volume := testVolume(t)
	r := testrepo.NewOnVolume(t, volume)
	hashes := map[string][32]byte{}
	for i := 0; i < 8; i++ {
		hash := writePayload(t, r, i)
		snapshot := r.Snapshot(i)
		hashes[snapshot.ID] = hash
	}
	before := r.Snapshots()
	r.Retention(r.Source, 2)
	filler := fillTestVolume(t, volume)
	cfg := r.Config()
	cfg.Apply = true
	cfg.ServiceReserve = 8 << 20
	ctx, cancel := context.WithTimeout(r.Context, 30*time.Second)
	defer cancel()
	result, code, err := app.RunPreflight(ctx, io.Discard, cfg)
	if err == nil || code != app.ExitRuntime || result.ReadyForBackup || len(result.Plan.Candidates) == 0 {
		t.Fatalf("full repository mutation must fail closed: code=%d error=%v", code, err)
	}
	journal, journalErr := os.ReadFile(cfg.OperationFile)
	if journalErr != nil {
		t.Fatal(journalErr)
	}
	attempted := result.Plan.Candidates[0].Snapshot.ID
	checkSingleIntent(t, journal, attempted)
	if err := os.Remove(filler); err != nil {
		t.Fatal(err)
	}
	after := snapshotIDs(r.Snapshots())
	for _, s := range before {
		if !slices.Contains(after, s.ID) {
			if s.ID != attempted {
				t.Fatal("failed operation lost an unrequested recovery point")
			}
			continue
		}
		restorePayload(t, r, s, hashes[s.ID])
	}
	for _, kept := range result.Plan.Kept {
		if !slices.Contains(after, kept.Snapshot.ID) {
			t.Fatal("protected recovery point was lost")
		}
	}
	if _, retryCode, retryErr := app.RunPreflight(r.Context, io.Discard, cfg); retryCode != app.ExitRuntime || retryErr == nil {
		t.Fatal("recovered free space restarted the interrupted operation")
	}
	if !slices.Equal(after, snapshotIDs(r.Snapshots())) {
		t.Fatal("retry consumed another deletion budget")
	}
	finalJournal, journalErr := os.ReadFile(cfg.OperationFile)
	if journalErr != nil || !bytes.Equal(journal, finalJournal) {
		t.Fatal("retry rewrote failed-operation evidence")
	}
}

func TestLinuxNormalMaintenanceDelayAndIsolatedReclamation(t *testing.T) {
	volume := testVolume(t)
	r := testrepo.NewOnVolume(t, volume)
	hashes := map[string][32]byte{}
	originals := map[string]kopia.Snapshot{}
	for i := 0; i < 8; i++ {
		hash := writePayload(t, r, i)
		snapshot := r.Snapshot(i)
		hashes[snapshot.ID] = hash
		originals[snapshot.ID] = snapshot
	}
	r.Retention(r.Source, 2)
	status, err := r.Client.Status(r.Context)
	if err != nil {
		t.Fatal(err)
	}
	cfg := r.Config()
	cfg.Apply = true
	cfg.MaxPasses = 3
	cfg.BatchSize = 1
	cfg.RunMaintenance = true
	cfg.MaintenanceMode = "full"
	cfg.SafetyMargin = status.FreeBytes() + 8<<20
	result, code, err := app.RunPreflight(r.Context, io.Discard, cfg)
	if err != nil || code != app.ExitInsufficientSpace || len(result.DeletedIDs) < 1 || len(result.DeletedIDs) > cfg.MaxPasses {
		t.Fatalf("normal delayed maintenance: code=%d deleted=%v error=%v", code, result.DeletedIDs, err)
	}
	afterNormal := r.Snapshots()
	for _, id := range result.DeletedIDs {
		if slices.Contains(snapshotIDs(afterNormal), id) {
			t.Fatal("deleted manifest remains")
		}
		destination := filepath.Join(r.Root, "not-yet-reclaimed-"+id)
		r.Run("snapshot", "restore", originals[id].RootEntry.ObjectID, destination)
		// An object-ID restore has no original root mode; Kopia makes it read-only.
		// This private directory must remain removable by the fixture cleanup.
		if err := os.Chmod(destination, 0o700); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(destination, "payload.bin"))
		if err != nil || sha256.Sum256(data) != hashes[id] {
			t.Fatal("normal safety unexpectedly discarded young content")
		}
	}
	if _, retryCode, retryErr := app.RunPreflight(r.Context, io.Discard, cfg); retryCode != app.ExitRuntime || retryErr == nil {
		t.Fatal("delayed reclamation restarted deletion budget")
	}
	if !slices.Equal(snapshotIDs(afterNormal), snapshotIDs(r.Snapshots())) {
		t.Fatal("retry deleted more manifests")
	}
	beforeReclaim, err := r.Client.Status(r.Context)
	if err != nil {
		t.Fatal(err)
	}
	// Only this private fixture uses SafetyNone: all clients are synchronous and
	// finished, its local backing image is re-attested, and no competing process exists.
	if testVolume(t) != volume || filepath.Dir(r.Storage) != volume {
		t.Fatal("reclamation fixture lost isolation")
	}
	// Native content timestamps have one-second resolution and advance
	// monotonically on rewrites. Immediate back-to-back cycles need not reclaim
	// their packs yet. Poll a measured outcome with a fixed small cycle budget;
	// no more manifests are deleted and no repository clock is changed.
	var reclaimed int64
	var maintenanceOutput strings.Builder
	for cycle := 0; cycle < 6; cycle++ {
		if cycle > 0 {
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context.Done():
				t.Fatal(r.Context.Err())
			}
		}
		maintenanceOutput.WriteString(r.Run("maintenance", "run", "--full", "--safety=none"))
		afterReclaim, err := r.Client.Status(r.Context)
		if err != nil {
			t.Fatal(err)
		}
		reclaimed = afterReclaim.FreeBytes() - beforeReclaim.FreeBytes()
		if reclaimed >= 2<<20 {
			break
		}
	}
	if reclaimed < 2<<20 {
		t.Fatalf("expected physical reclamation of unique 4MiB content, got %d bytes\n%s", reclaimed, maintenanceOutput.String())
	}
	remaining := r.Snapshots()
	if !slices.Equal(snapshotIDs(afterNormal), snapshotIDs(remaining)) {
		t.Fatal("maintenance changed retained manifest IDs")
	}
	for _, s := range remaining {
		restorePayload(t, r, s, hashes[s.ID])
	}
	t.Logf("normal safety retained deleted-manifest content; isolated accelerated maintenance reclaimed %d bytes; %d retained versions restored", reclaimed, len(remaining))
}
