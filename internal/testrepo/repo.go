// Package testrepo runs the real Kopia CLI only after explicit integration-test opt-in.
package testrepo

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/app"
	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/kopia"
)

type Repo struct {
	T       *testing.T
	Context context.Context
	Root    string
	Storage string
	Source  string
	Client  kopia.Client
	Status  kopia.RepositoryStatus
}

func New(t *testing.T) *Repo {
	t.Helper()
	if os.Getenv("KKS_INTEGRATION") != "1" {
		t.Skip("real Kopia tests require KKS_INTEGRATION=1 in an isolated environment")
	}
	binary := os.Getenv("KKS_KOPIA_BINARY")
	if binary == "" {
		var err error
		binary, err = exec.LookPath("kopia")
		if err != nil {
			t.Fatal("KKS_INTEGRATION=1 requires real Kopia; missing executable is a failure")
		}
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	r := &Repo{T: t, Context: ctx, Root: root, Storage: filepath.Join(root, "repo"), Source: filepath.Join(root, "sources", "source with spaces")}
	for _, name := range []string{r.Storage, r.Source, filepath.Join(root, "home"), filepath.Join(root, "cache"), filepath.Join(root, "tmp")} {
		if err := os.MkdirAll(name, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	home := filepath.Join(root, "home")
	env := []string{
		"HOME=" + home, "USERPROFILE=" + home, "APPDATA=" + home, "LOCALAPPDATA=" + home,
		"XDG_CONFIG_HOME=" + home, "XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
		"TMPDIR=" + filepath.Join(root, "tmp"), "TMP=" + filepath.Join(root, "tmp"), "TEMP=" + filepath.Join(root, "tmp"),
		"KOPIA_PASSWORD=synthetic-test-only", "KOPIA_PERSIST_CREDENTIALS_ON_CONNECT=false",
	}
	if runtime.GOOS == "windows" {
		env = append(env, "SystemRoot="+os.Getenv("SystemRoot"), "WINDIR="+os.Getenv("WINDIR"))
	}
	r.Client = kopia.Client{Binary: binary, ConfigFile: filepath.Join(root, "test.config"), Env: env}
	r.Run("repository", "create", "filesystem", "--path", r.Storage, "--override-username=test-user", "--override-hostname=test-host", "--no-enable-actions", "--no-check-for-updates")
	r.Status, err = r.Client.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r.Retention(r.Source, 100)
	return r
}

func (r *Repo) Command(ctx context.Context, args ...string) *exec.Cmd {
	r.T.Helper()
	base := []string{"--config-file", r.Client.ConfigFile, "--no-persist-credentials", "--no-auto-maintenance", "--disable-repository-log", "--disable-file-logging", "--disable-content-log", "--error-notifications=never", "--no-progress"}
	cmd := exec.CommandContext(ctx, r.Client.Binary, append(base, args...)...)
	cmd.Env = r.Client.Env
	cmd.Dir = r.Root
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func (r *Repo) Run(args ...string) string {
	r.T.Helper()
	out, err := r.Command(r.Context, args...).CombinedOutput()
	if err != nil {
		r.T.Fatalf("synthetic Kopia command %s failed: %v\n%s", args[0], err, out)
	}
	return string(out)
}

func (r *Repo) Retention(source string, latest int) {
	r.T.Helper()
	r.Run("policy", "set", source, "--keep-latest", strconv.Itoa(latest), "--keep-hourly=0", "--keep-daily=0", "--keep-weekly=0", "--keep-monthly=0", "--keep-annual=0")
}

func (r *Repo) Snapshot(index int, extra ...string) kopia.Snapshot {
	r.T.Helper()
	content := []byte(fmt.Sprintf("synthetic version %d\n", index))
	if err := os.WriteFile(filepath.Join(r.Source, "version.txt"), content, 0o600); err != nil {
		r.T.Fatal(err)
	}
	// Fixed distinct timestamps keep this integration test independent of CPU speed.
	when := time.Date(2026, 1, 1, 0, index, 0, 0, time.UTC).Format("2006-01-02 15:04:05 MST")
	args := []string{"snapshot", "create", r.Source, "--json", "--start-time", when, "--end-time", when, "--force-disable-actions", "--no-send-snapshot-report"}
	out := r.Run(append(args, extra...)...)
	var snapshot kopia.Snapshot
	// Snapshot diagnostics are on stderr. Use List rather than parsing CombinedOutput.
	_ = out
	snapshots := r.Snapshots()
	for _, s := range snapshots {
		if s.EndTime.Format("2006-01-02 15:04:05 MST") == when {
			snapshot = s
		}
	}
	if snapshot.ID == "" {
		r.T.Fatal("new snapshot missing")
	}
	return snapshot
}

func (r *Repo) Snapshots() []kopia.Snapshot {
	r.T.Helper()
	snapshots, err := r.Client.ListSnapshots(r.Context, r.Status.Source(r.Source))
	if err != nil {
		r.T.Fatal(err)
	}
	return snapshots
}

func (r *Repo) Config() app.Config {
	return app.Config{Source: r.Source, KopiaBinary: r.Client.Binary, KopiaConfigFile: r.Client.ConfigFile, KopiaEnv: r.Client.Env,
		RepositoryID: r.Status.ID, OperationFile: filepath.Join(r.Root, "operation.jsonl"),
		MaxPasses: 1, BatchSize: 2, KeepLatest: 1, EstimateMultiplier: 1, MaintenanceMode: "quick", ExclusiveAccess: true}
}

func (r *Repo) InsufficientMargin() int64 {
	r.T.Helper()
	gap := min(int64(1<<30), (r.Status.CapacityBytes()-r.Status.FreeBytes())/2)
	if gap < 64<<20 {
		r.T.Fatal("integration runner needs at least 128MiB of existing disk use to set a stable feasible headroom target without filling the disk")
	}
	return r.Status.FreeBytes() + gap
}

func (r *Repo) Restore(snapshot kopia.Snapshot, expected string) {
	r.T.Helper()
	destination := filepath.Join(r.Root, "restore-"+snapshot.ID)
	r.Run("snapshot", "restore", snapshot.ID, destination)
	data, err := os.ReadFile(filepath.Join(destination, "version.txt"))
	if err != nil || string(data) != expected {
		r.T.Fatalf("restore of %s: %v, content %q; want %q", snapshot.ID, err, data, expected)
	}
}

func (r *Repo) MaintenanceInfo() json.RawMessage {
	var info json.RawMessage
	output, err := r.Command(r.Context, "maintenance", "info", "--json").Output()
	if err != nil || json.Unmarshal(output, &info) != nil {
		r.T.Fatalf("maintenance info: %v", err)
	}
	return info
}
