package tools

import (
	"sort"
	"testing"
)

func TestTopN(t *testing.T) {
	counts := map[string]int{
		"alice@test.com": 10,
		"bob@test.com":   25,
		"carol@test.com": 5,
		"dave@test.com":  15,
	}

	result := topN(counts, 2)
	if len(result) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result))
	}
	if result[0] != "bob@test.com (25)" {
		t.Errorf("expected bob first, got %s", result[0])
	}
	if result[1] != "dave@test.com (15)" {
		t.Errorf("expected dave second, got %s", result[1])
	}
}

func TestTopNMoreThanAvailable(t *testing.T) {
	counts := map[string]int{
		"alice@test.com": 10,
	}

	result := topN(counts, 5)
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
}

func TestTopNEmpty(t *testing.T) {
	result := topN(map[string]int{}, 3)
	if len(result) != 0 {
		t.Fatalf("expected 0 results, got %d", len(result))
	}
}

func TestTopNStableSort(t *testing.T) {
	// When counts are equal, order is deterministic within a single run but
	// not guaranteed across runs. Just verify the count values are correct.
	counts := map[string]int{
		"a@test.com": 5,
		"b@test.com": 5,
		"c@test.com": 5,
	}

	result := topN(counts, 3)
	if len(result) != 3 {
		t.Fatalf("expected 3 results, got %d", len(result))
	}

	// All should end with " (5)".
	for _, r := range result {
		if len(r) < 4 {
			t.Errorf("result too short: %s", r)
		}
	}

	// Sort to verify all three are present.
	sort.Strings(result)
	if result[0] != "a@test.com (5)" || result[1] != "b@test.com (5)" || result[2] != "c@test.com (5)" {
		t.Errorf("unexpected results: %v", result)
	}
}
