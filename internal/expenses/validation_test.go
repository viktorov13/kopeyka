package expenses

import "testing"

func TestParseAmount(t *testing.T) {
	for input, want := range map[string]int64{"350": 35000, " 1,20 ": 120, "0.01": 1, "12.3": 1230, "999999999.99": 99999999999} {
		got, err := ParseAmount(input)
		if err != nil || got != want {
			t.Errorf("%q = %d,%v, want %d", input, got, err, want)
		}
	}
	for _, input := range []string{"", "0", "-1", "+5", "NaN", "1e3", "1.001", "1.", ".50", "1000000000", "9223372036854775808", "12 руб", "1.2.3"} {
		if _, err := ParseAmount(input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestValidation(t *testing.T) {
	valid := Input{UserID: 1, Amount: "0.01", Category: "еда", SpentAt: "2024-02-29"}
	if err := ValidateInput(valid); err != nil {
		t.Fatal(err)
	}
	invalidDate := valid
	invalidDate.SpentAt = "2025-02-29"
	if ValidateInput(invalidDate) == nil {
		t.Error("accepted invalid leap date")
	}
	invalidOwner := valid
	invalidOwner.UserID = 0
	if ValidateInput(invalidOwner) == nil {
		t.Error("accepted zero owner")
	}
	if ValidateFilter(Filter{From: "2026-02-02", To: "2026-02-01"}) == nil {
		t.Error("accepted reversed range")
	}
	if ValidateFilter(Filter{Category: "unknown"}) == nil {
		t.Error("accepted unknown category")
	}
}
