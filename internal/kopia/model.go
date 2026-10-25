package kopia

import (
	"encoding/hex"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"
)

type Source struct {
	Host     string `json:"host"`
	UserName string `json:"userName"`
	Path     string `json:"path"`
}

type RepositoryStatus struct {
	ConfigFile    string `json:"configFile"`
	ID            string `json:"uniqueIDHex"`
	ClientOptions struct {
		Hostname string `json:"hostname"`
		Username string `json:"username"`
		ReadOnly bool   `json:"readonly"`
	} `json:"clientOptions"`
	Storage struct {
		Type   string `json:"type"`
		Config struct {
			Path string `json:"path"`
		} `json:"config"`
	} `json:"storage"`
	Volume *struct {
		Capacity  *uint64 `json:"capacity"`
		Available *uint64 `json:"available"`
	} `json:"volume"`
}

func (s RepositoryStatus) Validate() error {
	if _, err := hex.DecodeString(s.ID); err != nil || s.ID == "" {
		return fmt.Errorf("repository status does not contain a valid repository ID")
	}
	if s.ClientOptions.Hostname == "" || s.ClientOptions.Username == "" {
		return fmt.Errorf("repository status does not contain a client identity")
	}
	if s.Storage.Type != "filesystem" || !filepath.IsAbs(s.Storage.Config.Path) {
		return fmt.Errorf("only a direct filesystem repository with an absolute storage path is supported")
	}
	if s.Volume == nil || s.Volume.Capacity == nil || s.Volume.Available == nil || *s.Volume.Capacity == 0 || *s.Volume.Capacity > math.MaxInt64 || *s.Volume.Available > *s.Volume.Capacity {
		return fmt.Errorf("repository status does not contain supported storage capacity")
	}
	return nil
}

func (s RepositoryStatus) CapacityBytes() int64 { return int64(*s.Volume.Capacity) }

func (s RepositoryStatus) FreeBytes() int64 {
	return int64(*s.Volume.Available)
}

func (s RepositoryStatus) Source(path string) Source {
	return Source{Host: s.ClientOptions.Hostname, UserName: s.ClientOptions.Username, Path: path}
}

type SnapshotStats struct {
	ErrorCount        int `json:"errorCount"`
	IgnoredErrorCount int `json:"ignoredErrorCount"`
}

type DirectorySummary struct {
	IncompleteReason  string `json:"incomplete,omitempty"`
	FatalErrorCount   int    `json:"numFailed"`
	IgnoredErrorCount int    `json:"numIgnoredErrors"`
}

type RootEntry struct {
	ObjectID string            `json:"obj"`
	Type     string            `json:"type"`
	Summary  *DirectorySummary `json:"summ"`
}

type Snapshot struct {
	ID               string            `json:"id"`
	Source           Source            `json:"source"`
	StartTime        time.Time         `json:"startTime"`
	EndTime          time.Time         `json:"endTime"`
	Tags             map[string]string `json:"tags,omitempty"`
	RetentionReasons []string          `json:"retentionReason,omitempty"`
	Pins             []string          `json:"pins,omitempty"`
	IncompleteReason string            `json:"incomplete,omitempty"`
	Stats            *SnapshotStats    `json:"stats"`
	RootEntry        *RootEntry        `json:"rootEntry"`
}

func ValidManifestID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 16 && id == strings.ToLower(id)
}

func (s Snapshot) Validate() error {
	if !ValidManifestID(s.ID) {
		return fmt.Errorf("snapshot list contains an invalid full manifest ID")
	}
	if s.Source.Path == "" || s.Source.Host == "" || s.Source.UserName == "" {
		return fmt.Errorf("snapshot list contains a missing source identity")
	}
	if s.StartTime.IsZero() || s.EndTime.IsZero() || s.EndTime.Before(s.StartTime) {
		return fmt.Errorf("snapshot %s has invalid timestamps", s.ID)
	}
	return nil
}

func (s Snapshot) Healthy() bool {
	return s.IncompleteReason == "" && s.Stats != nil &&
		s.Stats.ErrorCount == 0 && s.Stats.IgnoredErrorCount == 0 &&
		s.RootEntry != nil && s.RootEntry.ObjectID != "" && s.RootEntry.Type == "d" &&
		s.RootEntry.Summary != nil && s.RootEntry.Summary.IncompleteReason == "" &&
		s.RootEntry.Summary.FatalErrorCount == 0 && s.RootEntry.Summary.IgnoredErrorCount == 0
}
