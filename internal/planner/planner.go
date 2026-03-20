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
		return sorted[i].EndTime.Before(sorted[j].EndTime)
	})

	keepReasons := make(map[string]string, len(sorted))
	keepReasons[sorted[0].ID] = "oldest"
	keepReasons[sorted[len(sorted)-1].ID] = "latest"

	keepLatest := options.KeepLatest
	if keepLatest < 1 {
		keepLatest = 1
	}

	start := len(sorted) - keepLatest
	if start < 0 {
		start = 0
	}

	for i := start; i < len(sorted); i++ {
		if _, ok := keepReasons[sorted[i].ID]; !ok {
			keepReasons[sorted[i].ID] = "keep-latest window"
		}
	}

	for _, snapshot := range sorted {
		if len(snapshot.RetentionReasons) > 0 {
			keepReasons[snapshot.ID] = "retention-protected"
			continue
		}

		if matchesProtectedTag(snapshot.Tags, options.ProtectTags) {
			keepReasons[snapshot.ID] = "tag-protected"
		}
	}

	latestTime := sorted[len(sorted)-1].EndTime
	bucketChoices := map[int]candidateChoice{}
	for index, snapshot := range sorted {
		if _, kept := keepReasons[snapshot.ID]; kept {
			continue
		}

		age := latestTime.Sub(snapshot.EndTime)
		if age <= 0 {
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

		if tags[parts[0]] == parts[1] {
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
