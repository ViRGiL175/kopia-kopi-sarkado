package units

import (
	"math"
	"testing"
)

func TestHeadroomArithmeticRejectsOverflowWithoutRoundingDown(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		estimate              int64
		multiplier            float64
		reserve, margin, want int64
		invalid               bool
	}{
		{"fraction", 3, 1.5, 2, 3, 10, false},
		{"large exact", 9007199254740993, 1, 0, 0, 9007199254740993, false},
		{"max", math.MaxInt64, 1, 0, 0, math.MaxInt64, false},
		{"scaled overflow", math.MaxInt64, 2, 0, 0, 0, true},
		{"sum overflow", math.MaxInt64, 1, 1, 0, 0, true},
		{"nan", 1, math.NaN(), 0, 0, 0, true},
		{"infinity", 1, math.Inf(1), 0, 0, 0, true},
		{"negative", 1, 1, -1, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RequiredBytes(tc.estimate, tc.multiplier, tc.reserve, tc.margin)
			if (err != nil) != tc.invalid || (!tc.invalid && got != tc.want) {
				t.Fatalf("got %d, %v; want %d invalid=%t", got, err, tc.want, tc.invalid)
			}
		})
	}
	for _, input := range []string{"9223372036854775808B", "99999PiB", "NaN", "Inf", "-1"} {
		if _, err := ParseBytes(input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
	if got, err := ParseBytes("9007199254740993B"); err != nil || got != 9007199254740993 {
		t.Fatalf("integer rounded: %d %v", got, err)
	}
}
