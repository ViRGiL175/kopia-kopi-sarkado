# kopia-kopi-sarkado

Cross-platform Go CLI for Kopia preflight checks and logarithmic snapshot thinning when there may not be enough space for the next snapshot.

> [!IMPORTANT]
> This repository is LLM-assisted development.
> It is not my primary project, not my usual stack, and I do not have the time to maintain it as a long-term product.
> The feature still matters, so the repository is intentionally small and test-driven.

## Supported operation

The tool estimates the next backup, reads the exact source's snapshot history, and automatically prunes surplus snapshots when explicitly authorized. It does not deduplicate files or prove that two snapshots contain equivalent data.

This build requires **Kopia CLI 0.23.1** and a **direct filesystem repository**. Remote object storage and repository-server connections are rejected: their capacity cannot safely be inferred from local free disk space. Source and configuration paths must be absolute. Free bytes come from Kopia's repository status, for the connected storage.

`snapshot estimate` reports logical source size. Required headroom is `ceil(estimate * multiplier) + service_reserve + safety_margin`, with checked arithmetic. This remains a heuristic, not a promise that the backup will fit. A target above the actual total storage capacity blocks backup without deletion. Deleting manifests does not immediately free their content; the tool makes no reclaimable-byte prediction.

## Read-only preview

```sh
go run . plan --source /data/source --config-file /config/test.config
go run . preflight --source /data/source --config-file /config/test.config
```

Both commands default to no deletion. The complete report contains repository ID, exact source identity, every kept and candidate manifest ID, protection reasons, headroom, deletion budget, and maintenance scope. `plan` returns zero for a successfully produced plan even when headroom is insufficient; **never use `plan` as a backup gate**. `preflight` returns 2 for insufficient space.

Preview scans the specified source and reads repository metadata; Kopia may use its local cache. It does not create a backup, expire snapshots, or run maintenance.

## Automatic pruning

For a disposable test repository with externally serialized access:

```sh
go run . preflight \
  --source /data/source --config-file /config/test.config \
  --apply --repository-id ID_FROM_PREVIEW --exclusive-access \
  --operation-file /state/kopia-pruning-operation.jsonl \
  --max-passes 3 --batch-size 2
```

`--exclusive-access` confirms an operational prerequisite: **all writers, policy/pin editors, other pruning tools, and maintenance processes must be serialized externally for the entire preflight and subsequent backup**. The flag does not acquire a distributed lock. Kopia's separate CLI reads and explicit deletes do not provide an atomic compare-and-delete transaction. Revalidation detects observed changes; it cannot eliminate a change between the last check and deletion. Do not enable apply where exclusive access cannot be enforced.

The operation file must be on durable, trusted storage, outside the source and repository storage, with an existing private parent directory. It is created exclusively and never overwritten. The tool syncs the initial candidate/keep lists and budget, then each deletion intent and observed outcome. Protect the parent directory with OS permissions; on Windows the file inherits its directory ACL.

Use the **same operation path on every retry**. Any existing file blocks apply, including an interrupted or completed operation. Inspect the journal and actual repository state before a new operation. Never delete or rotate it automatically on failure, and never generate a fresh path for every scheduler retry. Rotate only after explicitly resolving the prior operation and confirming a successful backup. A read-only `preflight` can assess current headroom while the old journal remains.

One invocation deletes at most `max-passes * batch-size` of the originally listed candidates. Before each delete it rechecks repository identity, inventory, and current protection. It stops on cancellation, an unexpected inventory change, an error, an uncertain outcome, exhausted budget, or a batch with no measured free-space gain. It never expands the candidate set during an operation.

A nonzero CLI result may follow a committed deletion. The tool queries the repository again and reports confirmed deletions separately from unknown outcomes. An intent without a confirmed outcome after a crash requires inspection. The journal is a retry guard and audit trail, **not an undo log**. Durability still depends on the filesystem and hardware; abrupt power-loss behavior is not certified.

## What is preserved

Planning is scoped to the exact Kopia `host + userName + path` tuple. Ancestor sources returned by Kopia's path filter are excluded. Paths are not case-folded, Unicode-normalized, or merged through symlinks/hardlinks. If the same directory has multiple source identities, each is separate.

The tool preserves:

- Every snapshot protected by native Kopia retention reasons or pins.
- Incomplete, errored, ignored-error, or insufficiently described snapshots.
- The oldest complete snapshot, newest complete snapshot, and latest N complete snapshots.
- Snapshots matching a protected tag.
- A representative in each logarithmic age bucket, plus ties at the latest timestamp.

Incomplete snapshots do not satisfy the complete-snapshot floor. Metadata marked complete is not proof of readable content: periodically restore retained versions independently. Neither source files nor individual objects are directly deleted by this tool.

## Maintenance and recovery

Every subprocess disables Kopia's automatic maintenance, local file/content logging, and ordinary repository logging. Without `--run-maintenance`, manifest deletion is the only requested repository mutation.

`--run-maintenance --maintenance-mode quick|full` explicitly authorizes native Kopia maintenance after each batch. The client must already be the configured maintenance owner; the tool does not change ownership or schedules. Maintenance is **repository-wide**, including content unrelated to this source. Native maintenance explicitly enables its repository log manager even when ordinary repository logging is disabled; its logs and metadata are additional authorized writes. Its intent and success/uncertainty are journaled. No unsafe timing override or forced garbage collection is used.

Kopia's normal safety delays can prevent immediate reclamation even after full maintenance. If headroom remains insufficient, the backup stays blocked. Recover capacity separately; repeatedly deleting more recovery points is not a substitute for capacity planning. Deleted snapshots have no guaranteed undelete path, and maintenance can make their content unrecoverable. Test restoration before enabling pruning and maintain independent recovery copies.

## Flags and migration

- `--source`, `--config-file`: required absolute paths.
- `--kopia-bin`: executable, resolved once; use a trusted absolute path for automation.
- `--apply`, `--repository-id`, `--exclusive-access`, `--operation-file`: required together to authorize pruning.
- `--keep-latest`: complete snapshot floor, default 3, positive.
- `--max-passes`, `--batch-size`: defaults 3 and 2, each 1–100.
- `--estimate-multiplier`: finite and at least 1, default 1.25.
- `--service-reserve`, `--safety-margin`: default 512MiB and 1GiB.
- `--protect-tag`: repeatable exact JSON tag key/value, e.g. `--protect-tag tag:important=yes` for a Kopia `--tags important:yes` tag.
- `--run-maintenance`, `--maintenance-mode`: opt-in repository-wide maintenance, mode defaults to quick.

**Existing invocations must be updated.** Preflight no longer implicitly authorizes deletion. `--space-path` was removed to prevent checking an unrelated disk. `--password` was removed to avoid passwords in process arguments; use Kopia's configured credential mechanism or a scoped `KOPIA_PASSWORD` environment variable. Do not put credentials in shared scripts or logs. CLI errors omit subprocess output and arguments, and output capture is bounded.

Exit codes: 0 ready (or a successful plan), 2 insufficient space, 3 invalid configuration, 4 runtime failure or uncertain outcome. The CLI has a 30-minute deadline and handles Ctrl+C cancellation. Any nonzero preflight result must block the backup.

## Scheduler integration

Use `before-snapshot-root-action` in essential mode, with a timeout that covers the preflight deadline. Configure and test actions separately; this program does not enable them or modify schedules. The source path must match Kopia's recorded identity, including when a snapshot action remaps paths.

Keep exclusive access through the subsequent backup. Persist the operation path across failed attempts. A successful preflight only establishes measured headroom at that instant; new source data or other filesystem users can still exhaust it. Start with read-only runs and verify successful restoration from a disposable repository before authorizing writes.

## Development and verification

```sh
go build ./...
go test ./...
```

Without `KKS_INTEGRATION=1`, real integration tests explicitly skip; this is **not** a full safety check. To run them with Kopia 0.23.1 on an isolated runner:

```sh
KKS_INTEGRATION=1 go test -count=1 -v -timeout=10m ./...
```

PowerShell:

```powershell
$env:KKS_INTEGRATION = '1'
go test -count=1 -v -timeout=10m ./...
```

Optionally set `KKS_KOPIA_BINARY` to the absolute executable path. With integration enabled, missing Kopia fails the suite. Tests create disposable local repositories and synthetic files with separate config, password, home, cache, and temporary directories, and no inherited Kopia environment. Credential persistence and snapshot actions are disabled. No existing repository is used. Use isolated CI runners or a disposable machine; the test processes are not an OS sandbox.

The cross-platform suite exercises real retention JSON, pins, nested sources, incomplete uploads, dry-run, bounded application, restoration of retained versions, Unicode paths, hardlinks, symlinks on Unix, maintenance opt-in, concurrent pin changes, cancellation after partial success, and retry refusal. Arithmetic and argument validation use focused unit tests. No fake Kopia implementation is present.

A requested reserve above current free bytes but below total capacity exercises insufficient-headroom behavior without filling the disk. An impossible target above total capacity must not delete anything. POSIX operation creation syncs both file and parent directory; Windows uses file flushing.

### Linux process and filesystem checks

The additional required CI job runs `ci/linux-safety.sh` on a disposable hosted Ubuntu runner. It creates a **256MiB ext4 image**, attaches only that image through a loop device, and verifies the backing file and capacity before any fill operation. Tests keep configs, credentials, caches, journals and restored copies outside that small volume. Cleanup unmounts and detaches the exact owned device; if unmount fails, it leaves the image intact. The script refuses non-hosted runners and does not fill the runner's main filesystem.

This job verifies the wrapper's decisions and retries with real Kopia:

- Two processes which have already planned must not both claim the same operation file. This is an operation-file claim, not a repository-wide distributed lock.
- Changes to history, retention, and pins at deterministic checkpoints stop further pruning once observed.
- A real subprocess is killed with SIGKILL **after Kopia returns success but before the wrapper records the deletion result**. Exactly the intended manifest is absent, the journal retains an unresolved intent, and a fresh process using the same operation refuses more deletions. Remaining control versions restore correctly.
- An actually full test volume produces a real failed repository mutation. The wrapper stops after one intent, preserves protected recovery points, and does not restart the operation when space returns.
- Normal full maintenance preserves young deleted-manifest content under Kopia's default safety delays, while pruning/retry remains bounded. A separate, synchronous and exclusively owned test repository then runs full maintenance with `--safety=none` to measure actual reclaimed bytes and restore remaining control versions. **This test-only flag is never passed by the application or used with competing clients.** It demonstrates reclamation on isolated synchronous storage, not safety of bypassing delays in production.

Subprocess checkpoints use a test-only writer around the existing output interface: it waits for an explicit controller acknowledgement. The application only emits ordinary `operation: started` and `operation: delete-command-completed <id>` progress messages, has no test pause flags/environment, and still uses the actual Kopia executable. There are no timing guesses or mocked CLI responses at these checkpoints. `delete-command-completed` is not a durable confirmation; the journal and reconciliation determine the outcome.

Run both the cross-platform matrix and the Linux safety job for the full CI contract. Linux-specific cases intentionally skip outside their isolated job. Physical power cuts, hostile filesystem replacement, arbitrary cross-host writers and a 24-hour wall-clock maintenance soak are outside this suite. A VM power-cut experiment is not a gate for this wrapper: Kopia owns its storage consistency guarantees under its [documented storage requirements](https://kopia.io/docs/advanced/consistency/); these tests check the wrapper's selection, accounting and repeat behavior.

CI uploads no build artifacts or test repository images.
