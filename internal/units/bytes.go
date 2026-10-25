package units

import (
	"fmt"
	"math"
	"math/big"
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

	numberValue, ok := new(big.Rat).SetString(numberPart)
	if !ok {
		return 0, fmt.Errorf("invalid numeric size")
	}

	multiplier, ok := unitMultipliers[unitPart]
	if !ok {
		return 0, fmt.Errorf("unsupported unit %q", unitPart)
	}

	return ceilBytes(numberValue.Mul(numberValue, new(big.Rat).SetInt64(int64(multiplier))))
}

func RequiredBytes(estimate int64, multiplier float64, reserve, margin int64) (int64, error) {
	if estimate < 0 || reserve < 0 || margin < 0 || multiplier < 1 || math.IsNaN(multiplier) || math.IsInf(multiplier, 0) {
		return 0, fmt.Errorf("headroom values must be non-negative and multiplier must be finite and at least 1")
	}

	scaled := new(big.Rat).Mul(new(big.Rat).SetInt64(estimate), new(big.Rat).SetFloat64(multiplier))
	required, err := ceilBytes(scaled)
	if err != nil {
		return 0, err
	}
	for _, extra := range []int64{reserve, margin} {
		if extra > math.MaxInt64-required {
			return 0, fmt.Errorf("required headroom exceeds the supported range")
		}
		required += extra
	}

	return required, nil
}

func ceilBytes(value *big.Rat) (int64, error) {
	whole, remainder := new(big.Int), new(big.Int)
	whole.QuoRem(value.Num(), value.Denom(), remainder)
	if remainder.Sign() > 0 {
		whole.Add(whole, big.NewInt(1))
	}
	if value.Sign() < 0 || !whole.IsInt64() {
		return 0, fmt.Errorf("size is outside the supported range")
	}
	return whole.Int64(), nil
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
