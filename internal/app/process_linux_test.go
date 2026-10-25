//go:build linux

package app_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/app"
	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/kopia"
	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/planner"
	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/testrepo"
)

type processRequest struct {
	Config app.Config
	Gates  []string
}

type processEvent struct {
	Stage     string
	ID        string
	Code      int
	Deleted   []string
	Uncertain []string
	Error     string
}

type gatedOutput struct {
	input *bufio.Reader
	gates map[string]bool
}

func (w *gatedOutput) Write(p []byte) (int, error) {
	text := string(p)
	event := processEvent{}
	switch {
	case strings.HasPrefix(text, "repository-id:"):
		event.Stage = "plan"
	case text == "operation: started\n":
		event.Stage = "started"
	case strings.HasPrefix(text, "operation: delete-command-completed "):
		event.Stage = "delete-command-completed"
		event.ID = strings.TrimSpace(strings.TrimPrefix(text, "operation: delete-command-completed "))
	}
	if !w.gates[event.Stage] {
		return len(p), nil
	}
	delete(w.gates, event.Stage)
	if err := json.NewEncoder(os.Stdout).Encode(event); err != nil {
		return 0, err
	}
	line, err := w.input.ReadString('\n')
	if err != nil || line != "continue\n" {
		return 0, errors.New("test controller did not release checkpoint")
	}
	return len(p), nil
}

// The child runs the production preflight with real Kopia. Only the output writer
// is gated; no Kopia replies, repository operations, or journal writes are replaced.
func TestPruningProcess(t *testing.T) {
	if os.Getenv("KKS_PROCESS_HELPER") != "1" {
		return
	}
	if os.Getenv("KKS_INTEGRATION") != "1" || os.Getenv("KKS_LINUX_SAFETY") != "1" {
		os.Exit(90)
	}
	input := bufio.NewReader(os.Stdin)
	line, err := input.ReadBytes('\n')
	if err != nil {
		os.Exit(91)
	}
	var request processRequest
	if json.Unmarshal(line, &request) != nil {
		os.Exit(92)
	}
	gates := map[string]bool{}
	for _, stage := range request.Gates {
		gates[stage] = true
	}
	result, code, runErr := app.RunPreflight(t.Context(), &gatedOutput{input: input, gates: gates}, request.Config)
	event := processEvent{Stage: "done", Code: code, Deleted: result.DeletedIDs, Uncertain: result.UncertainIDs}
	if runErr != nil {
		event.Error = runErr.Error()
	}
	_ = json.NewEncoder(os.Stdout).Encode(event)
	os.Exit(code)
}

type preflightProcess struct {
	t       *testing.T
	repo    *testrepo.Repo
	command *exec.Cmd
	input   io.WriteCloser
	events  chan processEvent
	done    chan error
	stderr  bytes.Buffer
	waited  bool
}

func startPreflightProcess(t *testing.T, r *testrepo.Repo, cfg app.Config, gates ...string) *preflightProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := &preflightProcess{t: t, repo: r, events: make(chan processEvent, 8), done: make(chan error, 1)}
	p.command = exec.CommandContext(r.Context, executable, "-test.run=^TestPruningProcess$")
	p.command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	p.command.Dir = r.Root
	p.command.Env = append(append([]string{}, r.Client.Env...), "KKS_INTEGRATION=1", "KKS_LINUX_SAFETY=1", "KKS_PROCESS_HELPER=1")
	p.command.Stderr = &p.stderr
	p.input, err = p.command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := p.command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = p.command.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			var event processEvent
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				event = processEvent{Stage: "invalid-output", Error: scanner.Text()}
			}
			p.events <- event
		}
		close(p.events)
		p.done <- p.command.Wait()
	}()
	t.Cleanup(func() {
		_ = p.input.Close()
		if !p.waited {
			_ = syscall.Kill(-p.command.Process.Pid, syscall.SIGKILL)
			<-p.done
			p.waited = true
		}
	})
	if err := json.NewEncoder(p.input).Encode(processRequest{Config: cfg, Gates: gates}); err != nil {
		t.Fatal(err)
	}
	return p
}

func (p *preflightProcess) next(stage string) processEvent {
	p.t.Helper()
	select {
	case event, ok := <-p.events:
		if !ok || event.Stage != stage {
			p.t.Fatalf("expected child checkpoint %q, got %+v", stage, event)
		}
		return event
	case <-p.repo.Context.Done():
		p.t.Fatalf("child checkpoint %q timed out", stage)
	}
	return processEvent{}
}

func (p *preflightProcess) release() {
	p.t.Helper()
	if _, err := io.WriteString(p.input, "continue\n"); err != nil {
		p.t.Fatal(err)
	}
}

func (p *preflightProcess) wait(code int) processEvent {
	p.t.Helper()
	event := p.next("done")
	err := <-p.done
	p.waited = true
	got := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			p.t.Fatal(err)
		}
		got = exit.ExitCode()
	}
	if got != code || event.Code != code {
		p.t.Fatalf("child exit=%d event=%+v stderr=%s", got, event, p.stderr.String())
	}
	return event
}

func linuxSafety(t *testing.T) {
	t.Helper()
	if os.Getenv("KKS_LINUX_SAFETY") != "1" {
		t.Skip("requires isolated Linux safety job")
	}
}

func snapshotIDs(snapshots []kopia.Snapshot) []string {
	ids := make([]string, 0, len(snapshots))
	for _, s := range snapshots {
		ids = append(ids, s.ID)
	}
	slices.Sort(ids)
	return ids
}

func TestLinuxConcurrentProcessesClaimOneOperation(t *testing.T) {
	linuxSafety(t)
	r, cfg := prepared(t)
	before := r.Snapshots()
	first := startPreflightProcess(t, r, cfg, "plan", "started")
	first.next("plan")
	second := startPreflightProcess(t, r, cfg, "plan")
	second.next("plan")
	first.release()
	first.next("started")
	second.release()
	refused := second.wait(app.ExitRuntime)
	if len(refused.Deleted) != 0 || !strings.Contains(refused.Error, "exclusively") {
		t.Fatalf("second process did not lose exclusive claim: %+v", refused)
	}
	if !slices.Equal(snapshotIDs(before), snapshotIDs(r.Snapshots())) {
		t.Fatal("a held operation deleted early")
	}
	first.release()
	result := first.wait(app.ExitInsufficientSpace)
	if len(result.Deleted) != cfg.BatchSize {
		t.Fatalf("one owner must receive one bounded budget: %+v", result)
	}
	after := r.Snapshots()
	if len(after) != len(before)-len(result.Deleted) {
		t.Fatal("competing processes consumed extra deletion budget")
	}
	for _, s := range after {
		r.Restore(s, fmt.Sprintf("synthetic version %d\n", s.EndTime.Minute()))
	}
}

func TestLinuxObservedChangesBetweenStepsStopPruning(t *testing.T) {
	linuxSafety(t)
	for _, scenario := range []string{"new-history", "retention", "pin-after-delete"} {
		t.Run(scenario, func(t *testing.T) {
			r, cfg := prepared(t)
			candidates := planner.BuildPlan(r.Snapshots(), planner.Options{KeepLatest: cfg.KeepLatest, ProtectTags: cfg.ProtectTags}).Candidates
			if len(candidates) < 2 {
				t.Fatal("need two real candidates")
			}
			gate := "plan"
			if scenario == "pin-after-delete" {
				gate = "delete-command-completed"
			}
			process := startPreflightProcess(t, r, cfg, gate)
			event := process.next(gate)
			switch scenario {
			case "new-history":
				r.Retention(r.Source, 100)
				r.Snapshot(11)
			case "retention":
				r.Retention(r.Source, 100)
			case "pin-after-delete":
				r.Run("snapshot", "pin", "--add=concurrent-protection", candidates[1].Snapshot.ID)
			}
			afterExternalChange := snapshotIDs(r.Snapshots())
			process.release()
			result := process.wait(app.ExitRuntime)
			if !slices.Equal(afterExternalChange, snapshotIDs(r.Snapshots())) {
				t.Fatal("wrapper continued deletion after observed external change")
			}
			wantDeleted := 0
			if scenario == "pin-after-delete" {
				wantDeleted = 1
				if event.ID != candidates[0].Snapshot.ID || !slices.Contains(result.Deleted, event.ID) {
					t.Fatal("completed deletion was not reconciled")
				}
			}
			if len(result.Deleted) != wantDeleted {
				t.Fatalf("unexpected wrapper deletions: %+v", result)
			}
		})
	}
}

func TestLinuxKillAfterKopiaSuccessBeforeJournalResult(t *testing.T) {
	linuxSafety(t)
	r, cfg := prepared(t)
	cfg.BatchSize = 4
	before := r.Snapshots()
	candidates := planner.BuildPlan(before, planner.Options{KeepLatest: cfg.KeepLatest, ProtectTags: cfg.ProtectTags}).Candidates
	if len(candidates) < 2 {
		t.Fatal("need candidates beyond the first deletion")
	}
	process := startPreflightProcess(t, r, cfg, "delete-command-completed")
	completed := process.next("delete-command-completed")
	if completed.ID != candidates[0].Snapshot.ID {
		t.Fatal("deleted ID differs from original first candidate")
	}
	after := r.Snapshots()
	expected := slices.DeleteFunc(snapshotIDs(before), func(id string) bool { return id == completed.ID })
	if !slices.Equal(expected, snapshotIDs(after)) {
		t.Fatal("actual history differs from exactly one requested deletion")
	}
	journal, err := os.ReadFile(cfg.OperationFile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(journal, []byte(`"event":"delete-intent"`)) != 1 || bytes.Contains(journal, []byte("delete-confirmed")) || bytes.Contains(journal, []byte(`"event":"finished"`)) {
		t.Fatalf("not in the intended unrecorded-result window: %s", journal)
	}
	if err := process.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	killErr := <-process.done
	process.waited = true
	var exit *exec.ExitError
	if !errors.As(killErr, &exit) {
		t.Fatalf("expected forced process death: %v", killErr)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("not SIGKILL: %v", exit)
	}
	retry := startPreflightProcess(t, r, cfg)
	refused := retry.wait(app.ExitRuntime)
	if len(refused.Deleted) != 0 || !strings.Contains(refused.Error, "operation-file exists") {
		t.Fatalf("retry did not preserve unknown outcome: %+v", refused)
	}
	finalJournal, err := os.ReadFile(cfg.OperationFile)
	if err != nil || !bytes.Equal(journal, finalJournal) {
		t.Fatal("retry rewrote the interrupted operation")
	}
	if !slices.Equal(expected, snapshotIDs(r.Snapshots())) {
		t.Fatal("retry consumed a fresh or expanded deletion budget")
	}
	for _, s := range after {
		r.Restore(s, fmt.Sprintf("synthetic version %d\n", s.EndTime.Minute()))
	}
	t.Logf("SIGKILL after native success: exactly %s absent; one intent without result; retry refused", completed.ID)
}
