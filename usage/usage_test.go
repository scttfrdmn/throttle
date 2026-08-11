package usage_test

import (
	"encoding/json"
	"testing"

	"throttle/usage"
)

// An absent dimension and a dimension reported as zero are different facts: the
// first means "not consumed", the second means "the provider counted zero". Pricing
// treats them differently, so the distinction must survive.
func TestUsageAbsentIsNotZero(t *testing.T) {
	u := usage.New(map[usage.Dimension]int64{usage.CacheReadTokens: 0})

	if n, ok := u.Get(usage.CacheReadTokens); !ok || n != 0 {
		t.Errorf("Get(cache_read) = (%d, %v), want (0, true)", n, ok)
	}
	if n, ok := u.Get(usage.CacheWriteTokens); ok || n != 0 {
		t.Errorf("Get(cache_write) = (%d, %v), want (0, false)", n, ok)
	}
}

func TestUsageAddAndSet(t *testing.T) {
	var u usage.Usage // the zero value must be usable
	u.Add(usage.InputTokens, 100)
	u.Add(usage.InputTokens, 50)
	if got := u.Count(usage.InputTokens); got != 150 {
		t.Errorf("after two Adds, input = %d, want 150", got)
	}
	u.Set(usage.InputTokens, 7)
	if got := u.Count(usage.InputTokens); got != 7 {
		t.Errorf("after Set, input = %d, want 7", got)
	}
}

// A multi-step operation aggregates into one record, so Merge must sum rather
// than overwrite.
func TestUsageMerge(t *testing.T) {
	a := usage.New(map[usage.Dimension]int64{usage.InputTokens: 100, usage.OutputTokens: 20})
	b := usage.New(map[usage.Dimension]int64{usage.InputTokens: 5, usage.CacheReadTokens: 900})

	m := usage.Merge(a, b)
	for _, c := range []struct {
		d    usage.Dimension
		want int64
	}{
		{usage.InputTokens, 105},
		{usage.OutputTokens, 20},
		{usage.CacheReadTokens, 900},
	} {
		if got := m.Count(c.d); got != c.want {
			t.Errorf("merged %s = %d, want %d", c.d, got, c.want)
		}
	}

	// Merge must not mutate its inputs; a caller may still need the originals to
	// compare estimate against actual.
	if a.Count(usage.InputTokens) != 100 {
		t.Error("Merge mutated its left operand")
	}
}

// All() and Dimensions() hand out data that callers may hold, so they must be
// copies: a caller mutating a returned map must not corrupt the record.
func TestUsageAllIsACopy(t *testing.T) {
	u := usage.New(map[usage.Dimension]int64{usage.InputTokens: 100})
	all := u.All()
	all[usage.InputTokens] = 999
	if u.Count(usage.InputTokens) != 100 {
		t.Error("mutating All()'s result changed the usage record")
	}

	// New must copy its argument too.
	src := map[usage.Dimension]int64{usage.OutputTokens: 5}
	v := usage.New(src)
	src[usage.OutputTokens] = 999
	if v.Count(usage.OutputTokens) != 5 {
		t.Error("New retained a reference to its argument map")
	}
}

func TestUsageDimensionsAreSorted(t *testing.T) {
	u := usage.New(map[usage.Dimension]int64{
		usage.OutputTokens:    1,
		usage.InputTokens:     1,
		usage.CacheReadTokens: 1,
	})
	got := u.Dimensions()
	want := []usage.Dimension{usage.CacheReadTokens, usage.InputTokens, usage.OutputTokens}
	if len(got) != len(want) {
		t.Fatalf("Dimensions() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Dimensions() = %v, want %v", got, want)
		}
	}
}

// A provider that bills for something throttle has never heard of must still be
// representable, or the accounting silently drops a real charge.
func TestUsageAcceptsUnknownDimensions(t *testing.T) {
	const exotic usage.Dimension = "quantum_flops"
	u := usage.New(map[usage.Dimension]int64{exotic: 42})
	if got := u.Count(exotic); got != 42 {
		t.Errorf("unknown dimension count = %d, want 42", got)
	}
}

// A flat JSON map means a provider that starts reporting a new dimension needs no
// migration, and an old reader keeps a dimension it does not recognize.
func TestUsageJSONRoundTrip(t *testing.T) {
	u := usage.New(map[usage.Dimension]int64{
		usage.InputTokens:  1200,
		usage.OutputTokens: 350,
	})
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got, want := string(b), `{"input_tokens":1200,"output_tokens":350}`; got != want {
		t.Errorf("Marshal = %s, want %s", got, want)
	}

	var back usage.Usage
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Count(usage.InputTokens) != 1200 || back.Count(usage.OutputTokens) != 350 {
		t.Errorf("round trip lost data: %s", back)
	}
}

// A dimension written by a newer throttle must survive being read by an older
// one, rather than being dropped as unrecognized.
func TestUsageJSONPreservesUnknownDimensions(t *testing.T) {
	var u usage.Usage
	if err := json.Unmarshal([]byte(`{"input_tokens":10,"holograms":3}`), &u); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := u.Count("holograms"); got != 3 {
		t.Errorf("unknown dimension was dropped: %s", u)
	}

	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var again usage.Usage
	if err := json.Unmarshal(b, &again); err != nil {
		t.Fatalf("re-Unmarshal: %v", err)
	}
	if again.Count("holograms") != 3 {
		t.Error("an unrecognized dimension did not survive a re-marshal")
	}
}

func TestUsageEmptyJSON(t *testing.T) {
	var u usage.Usage
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(b) != "{}" {
		t.Errorf("Marshal(zero) = %s, want {}", b)
	}
}

// TotalTokens is display-only. The test pins that intent: it sums dimensions with
// different prices, so pricing must never be tempted to use it.
func TestTotalTokensIsDisplayOnly(t *testing.T) {
	u := usage.New(map[usage.Dimension]int64{
		usage.InputTokens:     100,
		usage.OutputTokens:    20,
		usage.CacheReadTokens: 900,
	})
	if got := u.TotalTokens(); got != 1020 {
		t.Errorf("TotalTokens = %d, want 1020", got)
	}
	// Non-token dimensions are excluded; seconds of audio are not tokens.
	u.Set(usage.AudioSeconds, 60)
	if got := u.TotalTokens(); got != 1020 {
		t.Errorf("TotalTokens counted a non-token dimension: %d", got)
	}
}
