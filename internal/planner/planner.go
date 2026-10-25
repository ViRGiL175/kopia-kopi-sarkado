package planner

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/kopia"
)

type Options struct {
	KeepLatest  int
	ProtectTags []string
}

type Decision struct {
	Snapshot kopia.Snapshot
	Reason   string
}

type Result struct {
	Kept       []Decision
	Candidates []Decision
}

type candidateChoice struct {
	Index     int
	Distance  float64
	Snapshot  kopia.Snapshot
	BucketKey int
}

func BuildPlan(snapshots []kopia.Snapshot, options Options) Result {
	if len(snapshots) == 0 {
		return Result{}
	}

	sorted := append([]kopia.Snapshot(nil), snapshots...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].EndTime.Equal(sorted[j].EndTime) {
			return sorted[i].ID < sorted[j].ID
		}
		return sorted[i].EndTime.Before(sorted[j].EndTime)
	})

	keepReasons := make(map[string]string, len(sorted))
	var healthy []kopia.Snapshot
	for _, snapshot := range sorted {
		if !snapshot.Healthy() {
			keepReasons[snapshot.ID] = "incomplete, errored, or unverified"
			continue
		}
		healthy = append(healthy, snapshot)
	}
	if len(healthy) == 0 {
		var result Result
		for _, snapshot := range sorted {
			result.Kept = append(result.Kept, Decision{Snapshot: snapshot, Reason: keepReasons[snapshot.ID]})
		}
		return result
	}
	keepReasons[healthy[0].ID] = "oldest complete"
	keepReasons[healthy[len(healthy)-1].ID] = "latest complete"

	keepLatest := options.KeepLatest
	if keepLatest < 1 {
		keepLatest = 1
	}

	start := len(healthy) - keepLatest
	if start < 0 {
		start = 0
	}

	for i := start; i < len(healthy); i++ {
		if _, ok := keepReasons[healthy[i].ID]; !ok {
			keepReasons[healthy[i].ID] = "keep-latest complete window"
		}
	}

	for _, snapshot := range sorted {
		if len(snapshot.Pins) > 0 {
			keepReasons[snapshot.ID] = "pinned"
			continue
		}
		if len(snapshot.RetentionReasons) > 0 {
			keepReasons[snapshot.ID] = "retention-protected"
			continue
		}

		if matchesProtectedTag(snapshot.Tags, options.ProtectTags) {
			keepReasons[snapshot.ID] = "tag-protected"
		}
	}

	latestTime := healthy[len(healthy)-1].EndTime
	bucketChoices := map[int]candidateChoice{}
	for index, snapshot := range sorted {
		if _, kept := keepReasons[snapshot.ID]; kept {
			continue
		}

		age := latestTime.Sub(snapshot.EndTime)
		if age <= 0 {
			keepReasons[snapshot.ID] = "latest timestamp tie"
			continue
		}

		bucketKey, distance := logBucket(age)
		choice, exists := bucketChoices[bucketKey]
		if !exists || distance < choice.Distance {
			bucketChoices[bucketKey] = candidateChoice{
				Index:     index,
				Distance:  distance,
				Snapshot:  snapshot,
				BucketKey: bucketKey,
			}
		}
	}

	for _, choice := range bucketChoices {
		if _, ok := keepReasons[choice.Snapshot.ID]; !ok {
			keepReasons[choice.Snapshot.ID] = "log-bucket representative"
		}
	}

	var result Result
	for _, snapshot := range sorted {
		reason, keep := keepReasons[snapshot.ID]
		decision := Decision{Snapshot: snapshot, Reason: reason}
		if keep {
			result.Kept = append(result.Kept, decision)
			continue
		}

		result.Candidates = append(result.Candidates, Decision{Snapshot: snapshot, Reason: "log-bucket surplus"})
	}

	return result
}

func matchesProtectedTag(tags map[string]string, protected []string) bool {
	if len(tags) == 0 || len(protected) == 0 {
		return false
	}

	for _, item := range protected {
		parts := strings.SplitN(item, "=", 2)
		if len(parts) != 2 {
			continue
		}

		if value, exists := tags[parts[0]]; exists && value == parts[1] {
			return true
		}
	}

	return false
}

func logBucket(age time.Duration) (int, float64) {
	base := 24 * time.Hour
	ratio := age.Seconds() / base.Seconds()
	if ratio <= 1 {
		center := 0.5
		return 0, math.Abs(ratio - center)
	}

	bucket := int(math.Floor(math.Log2(ratio))) + 1
	lower := math.Pow(2, float64(bucket-1))
	upper := math.Pow(2, float64(bucket))
	center := (lower + upper) / 2

	return bucket, math.Abs(ratio - center)
}
