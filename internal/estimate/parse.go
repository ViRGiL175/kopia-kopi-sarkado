package estimate

import (
	"fmt"
	"regexp"

	"github.com/ViRGiL175/kopia-kopi-sarkado/internal/units"
)

var includesPattern = regexp.MustCompile(`Snapshot includes \d+ file\(s\), total size ([^\r\n]+)`)

func ParseSnapshotEstimate(output string) (int64, error) {
	matches := includesPattern.FindStringSubmatch(output)
	if len(matches) != 2 {
		return 0, fmt.Errorf("unable to find included size in estimate output")
	}

	value, err := units.ParseBytes(matches[1])
	if err != nil {
		return 0, fmt.Errorf("parse estimate size: %w", err)
	}

	return value, nil
}
