package kopia

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/estimate"
)

type Client struct {
	Binary     string
	ConfigFile string
	Password   string
}

type Snapshot struct {
	ID               string            `json:"id"`
	StartTime        time.Time         `json:"startTime"`
	EndTime          time.Time         `json:"endTime"`
	Tags             map[string]string `json:"tags,omitempty"`
	RetentionReasons []string          `json:"retentionReasons,omitempty"`
}

func (c Client) EstimateSnapshotSize(ctx context.Context, source string) (int64, error) {
	stdout, _, err := c.run(ctx, "snapshot", "estimate", source)
	if err != nil {
		return 0, err
	}

	return estimate.ParseSnapshotEstimate(stdout)
}

func (c Client) ListSnapshots(ctx context.Context, source string) ([]Snapshot, error) {
	stdout, _, err := c.run(ctx, "snapshot", "list", "--json", source)
	if err != nil {
		return nil, err
	}

	var snapshots []Snapshot
	if err := json.Unmarshal([]byte(stdout), &snapshots); err != nil {
		return nil, fmt.Errorf("decode snapshot list json: %w", err)
	}

	return snapshots, nil
}

func (c Client) DeleteSnapshot(ctx context.Context, snapshotID string) error {
	_, _, err := c.run(ctx, "snapshot", "delete", snapshotID, "--delete")
	if err != nil {
		return err
	}

	return nil
}

func (c Client) RunMaintenance(ctx context.Context, mode string) error {
	args := []string{"maintenance", "run"}
	if strings.EqualFold(mode, "full") {
		args = append(args, "--full")
	}

	_, _, err := c.run(ctx, args...)
	return err
}

func (c Client) run(ctx context.Context, args ...string) (string, string, error) {
	binary := c.Binary
	if binary == "" {
		binary = "kopia"
	}

	baseArgs := []string{"--no-progress"}
	if c.ConfigFile != "" {
		baseArgs = append(baseArgs, "--config-file", c.ConfigFile)
	}

	cmd := exec.CommandContext(ctx, binary, append(baseArgs, args...)...)
	if c.Password != "" {
		cmd.Env = append(cmd.Environ(), "KOPIA_PASSWORD="+c.Password)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return stdout.String(), stderr.String(), fmt.Errorf("run %q: %w\nstdout:\n%s\nstderr:\n%s", strings.Join(cmd.Args, " "), err, stdout.String(), stderr.String())
	}

	return stdout.String(), stderr.String(), nil
}
