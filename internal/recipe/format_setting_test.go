package recipe

import "testing"

func TestFormatSettingValueKeepsLargeNumbersPlain(t *testing.T) {
	for in, want := range map[float64]string{1048576: "1048576", 2101248: "2101248", 262144: "262144", 0.5: "0.5", 12: "12"} {
		if got := formatSettingValue(in); got != want {
			t.Errorf("formatSettingValue(%v) = %q, want %q", in, got, want)
		}
	}
}
