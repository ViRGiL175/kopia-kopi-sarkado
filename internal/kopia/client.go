package kopia

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/estimate"
)

type Client struct {
	Binary     string
	ConfigFile string
	// A non-nil environment replaces the inherited environment (used by isolated integration tests).
	Env []string
}

func (c Client) Resolve() (Client, error) {
	if !filepath.IsAbs(c.ConfigFile) {
		return c, errors.New("an absolute config-file is required")
	}
	if c.Binary == "" {
		c.Binary = "kopia"
	}
	binary, err := exec.LookPath(c.Binary)
	if err != nil {
		return c, errors.New("Kopia executable was not found")
	}
	c.Binary, err = filepath.Abs(binary)
	return c, err
}

func (c Client) Status(ctx context.Context) (RepositoryStatus, error) {
	var status RepositoryStatus
	if err := c.readJSON(ctx, &status, "repository", "status", "--json"); err != nil {
		return status, err
	}
	return status, status.Validate()
}

func (c Client) CheckVersion(ctx context.Context) error {
	output, _, err := c.run(ctx, "--version")
	if err != nil {
		return err
	}
	fields := strings.Fields(output)
	if len(fields) == 0 || fields[0] != "0.23.1" {
		return errors.New("unsupported Kopia version; this build requires the integration-tested CLI 0.23.1")
	}
	return nil
}

func (c Client) EstimateSnapshotSize(ctx context.Context, source string) (int64, error) {
	stdout, stderr, err := c.run(ctx, "snapshot", "estimate", "--quiet", "--", source)
	if err != nil {
		return 0, err
	}
	if strings.TrimSpace(stderr) != "" {
		return 0, errors.New("Kopia estimate emitted diagnostics; refusing an uncertain estimate")
	}
	return estimate.ParseSnapshotEstimate(stdout)
}

func (c Client) ListSnapshots(ctx context.Context, source Source) ([]Snapshot, error) {
	var all []Snapshot
	if err := c.readJSON(ctx, &all, "snapshot", "list", "--json", "--", source.Path); err != nil {
		return nil, err
	}
	var snapshots []Snapshot
	seen := map[string]bool{}
	for _, snapshot := range all {
		if err := snapshot.Validate(); err != nil {
			return nil, err
		}
		if seen[snapshot.ID] {
			return nil, errors.New("snapshot list contains duplicate manifest IDs")
		}
		seen[snapshot.ID] = true
		// Kopia's source filter includes ancestors. Never merge their retention floors.
		if snapshot.Source == source {
			snapshots = append(snapshots, snapshot)
		}
	}
	return snapshots, nil
}

func (c Client) DeleteSnapshot(ctx context.Context, snapshotID string) error {
	if !ValidManifestID(snapshotID) {
		return errors.New("refusing a non-manifest snapshot ID")
	}
	_, _, err := c.run(ctx, "snapshot", "delete", snapshotID, "--delete")
	return err
}

func (c Client) CheckMaintenanceOwner(ctx context.Context, status RepositoryStatus) error {
	var info struct {
		Owner string `json:"owner"`
	}
	if err := c.readJSON(ctx, &info, "maintenance", "info", "--json"); err != nil {
		return err
	}
	if info.Owner != status.ClientOptions.Username+"@"+status.ClientOptions.Hostname {
		return errors.New("this client is not the maintenance owner")
	}
	return nil
}

func (c Client) RunMaintenance(ctx context.Context, mode string) error {
	args := []string{"maintenance", "run"}
	switch mode {
	case "full":
		args = append(args, "--full")
	case "quick":
	default:
		return errors.New("invalid maintenance mode")
	}
	_, _, err := c.run(ctx, args...)
	return err
}

func (c Client) readJSON(ctx context.Context, dst any, args ...string) error {
	stdout, stderr, err := c.run(ctx, args...)
	if err != nil {
		return err
	}
	// snapshot list may succeed after logging a policy evaluation failure.
	if strings.TrimSpace(stderr) != "" {
		return errors.New("Kopia emitted diagnostics while reading metadata; refusing to prune")
	}
	if err := json.Unmarshal([]byte(stdout), dst); err != nil {
		return errors.New("invalid Kopia metadata JSON")
	}
	return nil
}

type boundedOutput struct {
	bytes.Buffer
	limit    int
	cancel   context.CancelFunc
	exceeded bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		b.exceeded = true
		b.cancel()
		return 0, errors.New("Kopia output limit exceeded")
	}
	return b.Buffer.Write(p)
}

func (c Client) run(ctx context.Context, args ...string) (string, string, error) {
	if !filepath.IsAbs(c.Binary) || !filepath.IsAbs(c.ConfigFile) {
		return "", "", errors.New("resolve an explicit Kopia executable and config before use")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	baseArgs := []string{"--no-progress", "--no-auto-maintenance", "--disable-repository-log", "--disable-file-logging", "--disable-content-log", "--error-notifications=never", "--config-file", c.ConfigFile}
	cmd := exec.CommandContext(ctx, c.Binary, append(baseArgs, args...)...)
	cmd.Env = c.Env
	cmd.WaitDelay = 5 * time.Second
	stdout := &boundedOutput{limit: 64 << 20, cancel: cancel}
	stderr := &boundedOutput{limit: 64 << 10, cancel: cancel}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if stdout.exceeded || stderr.exceeded {
		return "", "", errors.New("Kopia output limit exceeded; operation outcome may be uncertain")
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		// CLI streams and arguments may include paths or credentials. Never echo them.
		return "", "", fmt.Errorf("Kopia %s failed (exit code %d); operation outcome may be uncertain", args[0], cmdExitCode(cmd))
	}
	return stdout.String(), stderr.String(), nil
}

func cmdExitCode(cmd *exec.Cmd) int {
	if cmd.ProcessState == nil {
		return -1
	}
	return cmd.ProcessState.ExitCode()
}
