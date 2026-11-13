package lks

import "testing"

func TestNormalizeWeekKey(t *testing.T) {
	tests := map[string]string{
		"чн":          "ch",
		"ch":          "ch",
		"числитель":   "ch",
		"зн":          "zn",
		"zn":          "zn",
		"знаменатель": "zn",
	}
	for input, want := range tests {
		got, err := normalizeWeekKey(input)
		if err != nil || got != want {
			t.Fatalf("normalizeWeekKey(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := normalizeWeekKey("unknown"); err == nil {
		t.Fatal("expected unknown week key to fail")
	}
}
