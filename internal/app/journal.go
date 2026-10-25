package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type journalEvent struct {
	Event     string   `json:"event"`
	ID        string   `json:"id,omitempty"`
	Mode      string   `json:"mode,omitempty"`
	Deleted   []string `json:"deleted,omitempty"`
	Uncertain []string `json:"uncertain,omitempty"`
	ExitCode  *int     `json:"exitCode,omitempty"`
}

type operationJournal struct {
	*os.File
}

func createJournal(path string, result Result, cfg Config) (*operationJournal, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, errors.New("cannot create operation-file exclusively; an existing operation must be inspected, never overwritten or automatically retried")
	}
	j := &operationJournal{File: f}
	var candidates []string
	var kept []string
	for _, d := range result.Plan.Candidates {
		candidates = append(candidates, d.Snapshot.ID)
	}
	for _, d := range result.Plan.Kept {
		kept = append(kept, d.Snapshot.ID)
	}
	header := struct {
		Event         string    `json:"event"`
		Created       time.Time `json:"created"`
		RepositoryID  string    `json:"repositoryID"`
		Source        any       `json:"source"`
		Candidates    []string  `json:"candidates"`
		Kept          []string  `json:"kept"`
		DeleteBudget  int       `json:"deleteBudget"`
		RequiredBytes int64     `json:"requiredBytes"`
		Maintenance   bool      `json:"maintenance"`
		Mode          string    `json:"mode"`
	}{"operation", time.Now().UTC(), result.RepositoryID, result.Source, candidates, kept, cfg.MaxPasses * cfg.BatchSize, result.RequiredBytes, cfg.RunMaintenance, cfg.MaintenanceMode}
	if err := j.record(header); err != nil {
		_ = f.Close()
		return nil, err
	}
	// File.Sync alone does not persist a newly created directory entry on POSIX.
	if runtime.GOOS != "windows" {
		dir, err := os.Open(filepath.Dir(path))
		if err == nil {
			err = dir.Sync()
			err = errors.Join(err, dir.Close())
		}
		if err != nil {
			_ = f.Close()
			return nil, errors.New("cannot sync operation directory; stopping before deletion")
		}
	}
	return j, nil
}

func (j *operationJournal) record(event any) error {
	if err := json.NewEncoder(j.File).Encode(event); err != nil {
		return errors.New("cannot append operation journal; stopping")
	}
	if err := j.Sync(); err != nil {
		return errors.New("cannot sync operation journal; stopping")
	}
	return nil
}

func validateJournalLocation(path, source, storage string) error {
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return errors.New("operation-file parent must exist and be accessible")
	}
	for _, root := range []string{source, storage} {
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return errors.New("cannot resolve source or repository storage path")
		}
		rel, err := filepath.Rel(resolved, parent)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return errors.New("operation-file must be outside both source and repository storage")
		}
	}
	return nil
}
