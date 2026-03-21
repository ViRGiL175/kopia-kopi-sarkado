# kopia-kopi-sarkado

Cross-platform Go CLI for Kopia preflight checks and logarithmic snapshot thinning when there may not be enough space for the next snapshot.

> [!IMPORTANT]
> This repository is LLM-assisted development.
> It is not my primary project, not my usual stack, and I do not have the time to maintain it as a long-term product.
> The feature still matters, so the repository is intentionally small and test-driven.

## MVP

1. Uses `kopia snapshot estimate` as a heuristic for the next snapshot size.
2. Computes required headroom as `estimate * multiplier + service_reserve + safety_margin`.
3. Reads snapshot history via `kopia snapshot list --json`.
4. Builds a logarithmic pruning plan.
5. In `preflight` mode, fails early if even optimistic full pruning still cannot reach the target headroom.
6. Otherwise, deletes snapshots in batches between passes.
7. Re-measures actual free space after each pass.
8. Fails if the target headroom is still not reached after `max-passes`.

## Estimate

`kopia snapshot estimate` is useful for preflight logic, but it is not a guaranteed upper bound.

1. It must not be treated as the exact amount of space that must be freed.
2. The stop condition must be based on measured free space after each pass.
3. Deleting snapshots does not guarantee proportional space reclamation because of deduplication and maintenance behavior.

## Why Go And Why An External CLI

1. Go gives a single binary across platforms.
2. Kopia does not expose a convenient plugin model for this kind of pruning engine.
3. An external CLI is easier to integrate, test, and run as a pre-hook.
4. Integration stays on top of the supported Kopia CLI instead of patching Kopia itself.

## Implemented Commands

### `plan`

Builds a pruning plan and reports the required headroom without deleting snapshots.

Example:

```sh
go run . plan --source /data/source --space-path /repo
```

### `preflight`

Runs the pruning loop until the safe threshold is reached or the command fails.

Example:

```sh
go run . preflight --source /data/source --space-path /repo --max-passes 3 --batch-size 2 --run-maintenance
```

## Main Flags

1. `--source` path of the source already backed up by Kopia.
2. `--space-path` path on the same filesystem where free space should be measured.
3. `--password` repository password when it is not stored in config or a credential manager.
4. `--estimate-multiplier` multiplier applied on top of the estimate.
5. `--safety-margin` extra headroom, for example `1GiB`.
6. `--service-reserve` reserve for Kopia metadata and temporary writes.
7. `--max-passes` maximum number of pruning passes.
8. `--batch-size` number of snapshots deleted per pass.
9. `--keep-latest` number of newest snapshots that are never touched.
10. `--run-maintenance` whether maintenance should run after each pass.
11. `--maintenance-mode` `quick` or `full`.
12. `--protect-tag` snapshot tag that must never be deleted, format `key=value`.

## Pruning Logic

The tool always preserves:

1. The oldest snapshot.
2. The newest snapshot.
3. The latest `N` snapshots.
4. Snapshots with protected tags.
5. One representative snapshot per logarithmic age bucket.

Everything else becomes a deletion candidate.

The tool does not delete beyond that retention floor.
If `free_space + optimistic_reclaimable_headroom` is still below the target, preflight aborts before deleting anything.
If all candidates are exhausted and there is still not enough space, preflight fails and the backup stays blocked.

## Integration Tests

The project includes real integration tests against a temporary Kopia filesystem repository.
Low-space scenarios use an injected free-space probe so snapshot operations stay real while the available-space behavior remains deterministic.

1. Positive path: enough free space already exists, so backup is allowed and nothing is deleted.
2. Early-impossible path: even optimistic full pruning is insufficient, so nothing is deleted and backup remains blocked.
3. Bounded-pass path: some snapshots are deleted, but `max-passes` is reached first, so backup remains blocked.
4. Retention-floor path: all disposable candidates are exhausted, but protected snapshots are still preserved and backup remains blocked.

## How To Integrate With The Kopia Scheduler

The intended integration is a pre-hook via `before-snapshot-root-action`.

Flow:

1. Kopia scheduler starts a snapshot job.
2. `before-snapshot-root-action` runs `kopia-kopi-sarkado preflight ...`.
3. If the command returns `0`, the backup proceeds.
4. If the command returns a non-zero exit code, the backup does not start.

Practical notes:

1. Use `essential` action mode.
2. Set `action-command-timeout` high enough for the preflight duration.
3. Keep the pruning loop bounded for large repositories.

## MVP Limitations

1. Free-space measurement currently requires a local `--space-path`.
2. The tool works through the Kopia CLI, not through Kopia internal APIs.
3. `snapshot estimate` is only a heuristic.
4. Space reclamation after deletion is probabilistic, not guaranteed.

## Project Structure

1. [main.go](main.go) entry point.
2. [internal/cli/cli.go](internal/cli/cli.go) command and flag parsing.
3. [internal/app/preflight.go](internal/app/preflight.go) preflight logic and pruning loop.
4. [internal/kopia/client.go](internal/kopia/client.go) Kopia CLI calls.
5. [internal/planner/planner.go](internal/planner/planner.go) logarithmic planner.
6. [internal/storage/free_windows.go](internal/storage/free_windows.go) and [internal/storage/free_unix.go](internal/storage/free_unix.go) free-space measurement.
7. [internal/estimate/parse.go](internal/estimate/parse.go) parsing for `snapshot estimate` output.
8. [internal/units/bytes.go](internal/units/bytes.go) size parsing and formatting.
9. [integration_test.go](integration_test.go) real end-to-end tests with a temporary Kopia repository.

## Development

Build:

```sh
go build ./...
```

Test:

```sh
go test ./...
```

## CI

1. Pull requests.
2. Manual `workflow_dispatch` runs.
3. `act`, using the Linux job locally.
