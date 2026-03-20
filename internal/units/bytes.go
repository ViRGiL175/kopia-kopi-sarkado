package units

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

var unitMultipliers = map[string]float64{
	"B":   1,
	"KB":  1000,
	"MB":  1000 * 1000,
	"GB":  1000 * 1000 * 1000,
	"TB":  1000 * 1000 * 1000 * 1000,
	"PB":  1000 * 1000 * 1000 * 1000 * 1000,
	"KIB": 1024,
	"MIB": 1024 * 1024,
	"GIB": 1024 * 1024 * 1024,
	"TIB": 1024 * 1024 * 1024 * 1024,
	"PIB": 1024 * 1024 * 1024 * 1024 * 1024,
	"K":   1000,
	"M":   1000 * 1000,
	"G":   1000 * 1000 * 1000,
	"KI":  1024,
	"MI":  1024 * 1024,
	"GI":  1024 * 1024 * 1024,
}

func ParseBytes(value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, fmt.Errorf("empty size")
	}

	trimmed = strings.ReplaceAll(trimmed, " ", "")
	index := 0
	for index < len(trimmed) {
		ch := trimmed[index]
		if (ch >= '0' && ch <= '9') || ch == '.' {
			index++
			continue
		}

		break
	}

	if index == 0 {
		return 0, fmt.Errorf("invalid size %q", value)
	}

	numberPart := trimmed[:index]
	unitPart := strings.ToUpper(trimmed[index:])
	if unitPart == "" {
		unitPart = "B"
	}

	numberValue, err := strconv.ParseFloat(numberPart, 64)
	if err != nil {
		return 0, fmt.Errorf("parse size %q: %w", value, err)
	}

	multiplier, ok := unitMultipliers[unitPart]
	if !ok {
		return 0, fmt.Errorf("unsupported unit %q", unitPart)
	}

	return int64(math.Ceil(numberValue * multiplier)), nil
}

func MustParseBytes(value string) int64 {
	v, err := ParseBytes(value)
	if err != nil {
		panic(err)
	}

	return v
}

func FormatBytes(value int64) string {
	if value < 1024 {
		return fmt.Sprintf("%d B", value)
	}

	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	result := float64(value)
	unitIndex := -1
	for result >= 1024 && unitIndex < len(units)-1 {
		result /= 1024
		unitIndex++
	}

	if unitIndex < 0 {
		return fmt.Sprintf("%d B", value)
	}

	return fmt.Sprintf("%.2f %s", result, units[unitIndex])
}
