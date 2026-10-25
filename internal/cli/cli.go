package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/app"
	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/units"
)

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSliceFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return app.ExitUsage
	}

	switch args[0] {
	case "preflight":
		return runPreflight(ctx, args[1:], stdout, stderr)
	case "plan":
		return runPlan(ctx, args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return app.ExitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		printUsage(stderr)
		return app.ExitUsage
	}
}

func runPreflight(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	config, ok := parseConfig(args, stderr)
	if !ok {
		return app.ExitUsage
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	result, code, err := app.RunPreflight(ctx, stdout, config)
	reportResult(stdout, result, code)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return code
	}

	return code
}

func runPlan(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	config, ok := parseConfig(args, stderr)
	if !ok {
		return app.ExitUsage
	}

	if config.Apply {
		fmt.Fprintln(stderr, "plan does not accept --apply")
		return app.ExitUsage
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	result, code, err := app.RunPreflight(ctx, stdout, config)
	reportResult(stdout, result, code)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return code
	}

	if code == app.ExitInsufficientSpace {
		return app.ExitOK
	}

	return code
}

func parseConfig(args []string, stderr io.Writer) (app.Config, bool) {
	fs := flag.NewFlagSet("preflight", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var protectTags stringSliceFlag
	var safetyMarginValue string
	var serviceReserveValue string

	config := app.Config{}
	fs.StringVar(&config.Source, "source", "", "Source path known to Kopia")
	fs.StringVar(&config.KopiaBinary, "kopia-bin", "kopia", "Path to kopia binary")
	fs.StringVar(&config.KopiaConfigFile, "config-file", "", "Path to Kopia config file")
	fs.BoolVar(&config.Apply, "apply", false, "Allow bounded automatic snapshot deletion")
	fs.BoolVar(&config.ExclusiveAccess, "exclusive-access", false, "Confirm external serialization of every repository writer and policy change")
	fs.StringVar(&config.RepositoryID, "repository-id", "", "Expected repository ID (required with --apply)")
	fs.StringVar(&config.OperationFile, "operation-file", "", "New durable operation journal outside source and storage; reuse this path on retries")
	fs.IntVar(&config.MaxPasses, "max-passes", 3, "Maximum prune passes")
	fs.IntVar(&config.BatchSize, "batch-size", 2, "Snapshots to delete per pass")
	fs.IntVar(&config.KeepLatest, "keep-latest", 3, "Always keep the latest N snapshots")
	fs.Float64Var(&config.EstimateMultiplier, "estimate-multiplier", 1.25, "Multiplier applied to snapshot estimate")
	fs.StringVar(&safetyMarginValue, "safety-margin", "1GiB", "Additional safety margin")
	fs.StringVar(&serviceReserveValue, "service-reserve", "512MiB", "Reserve kept for Kopia metadata and temp writes")
	fs.BoolVar(&config.RunMaintenance, "run-maintenance", false, "Run maintenance after each pass")
	fs.StringVar(&config.MaintenanceMode, "maintenance-mode", "quick", "Maintenance mode: quick or full")
	fs.Var(&protectTags, "protect-tag", "Snapshot tag to always preserve, format key=value; repeatable")

	if err := fs.Parse(args); err != nil {
		return app.Config{}, false
	}

	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "unexpected positional arguments")
		return app.Config{}, false
	}

	safetyMargin, err := units.ParseBytes(safetyMarginValue)
	if err != nil {
		fmt.Fprintf(stderr, "invalid safety-margin: %v\n", err)
		return app.Config{}, false
	}

	serviceReserve, err := units.ParseBytes(serviceReserveValue)
	if err != nil {
		fmt.Fprintf(stderr, "invalid service-reserve: %v\n", err)
		return app.Config{}, false
	}

	config.SafetyMargin = safetyMargin
	config.ServiceReserve = serviceReserve
	config.ProtectTags = protectTags
	if err := config.Validate(); err != nil {
		fmt.Fprintln(stderr, err)
		return app.Config{}, false
	}

	return config, true
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "kopia-kopi-sarkado")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  plan       Build a pruning plan and report required headroom")
	fmt.Fprintln(w, "  preflight  Check headroom; --apply enables bounded automatic pruning")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Common flags:")
	fmt.Fprintln(w, "  --source <path>          Source path already tracked by Kopia")
	fmt.Fprintln(w, "  --config-file <path>     Explicit absolute Kopia configuration path")
	fmt.Fprintln(w, "  --apply                 Authorize deletion (preflight only)")
	fmt.Fprintln(w, "  --repository-id <id>     Bind deletion to the reviewed repository ID")
	fmt.Fprintln(w, "  --operation-file <path>  Durable one-operation journal; never rotate on retries")
	fmt.Fprintln(w, "  --estimate-multiplier    Extra multiplier over snapshot estimate")
	fmt.Fprintln(w, "  --safety-margin <size>   Extra margin like 1GiB")
	fmt.Fprintln(w, "  --service-reserve <size> Reserve for Kopia metadata and temp writes")
}

func reportResult(w io.Writer, result app.Result, code int) {
	for _, id := range result.DeletedIDs {
		fmt.Fprintf(w, "confirmed-deleted: %s\n", id)
	}
	for _, id := range result.UncertainIDs {
		fmt.Fprintf(w, "deletion-unknown: %s\n", id)
	}
	fmt.Fprintf(w, "ready: %t\nexit-code: %d\n", result.ReadyForBackup, code)
}
