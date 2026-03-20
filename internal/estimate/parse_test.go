package estimate

import "testing"

func TestParseSnapshotEstimate(t *testing.T) {
	output := "Snapshot includes 42 file(s), total size 1.50 GiB\nEstimated upload time: 5m0s at 10 Mbit/s\n"

	got, err := ParseSnapshotEstimate(output)
	if err != nil {
		t.Fatalf("ParseSnapshotEstimate() error = %v", err)
	}

	const want int64 = 1610612736
	if got != want {
		t.Fatalf("ParseSnapshotEstimate() = %d, want %d", got, want)
	}
}
