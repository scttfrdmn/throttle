package usage

import (
	"encoding/json"
	"fmt"

	"github.com/scttfrdmn/throttle/money"
)

// Usage persists as a flat object of dimension -> count, e.g.
//
//	{"input_tokens":1200,"output_tokens":350}
//
// A flat map means a provider that starts reporting a new dimension needs no
// migration, and an old throttle reading a newer record keeps the dimension it
// does not recognize rather than dropping a real charge.

// MarshalJSON implements json.Marshaler.
func (u Usage) MarshalJSON() ([]byte, error) {
	if u.dims == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(u.dims)
}

// UnmarshalJSON implements json.Unmarshaler.
func (u *Usage) UnmarshalJSON(b []byte) error {
	var dims map[Dimension]int64
	if err := json.Unmarshal(b, &dims); err != nil {
		return err
	}
	if len(dims) == 0 {
		u.dims = nil
		return nil
	}
	u.dims = dims
	return nil
}

// Cost persists as an object rather than a bare number, because "unknown" has to
// survive the round trip: a null or absent amount must not read back as zero.
//
// Completeness is written explicitly. A partial amount that came back looking
// like a known total would be a silent understatement of real spend, which is the
// exact failure the type exists to prevent.
type costJSON struct {
	Amount       *int64       `json:"amount,omitempty"`
	Completeness Completeness `json:"completeness,omitempty"`
	Unpriced     []Dimension  `json:"unpriced,omitempty"`
	Reason       string       `json:"reason,omitempty"`
}

// MarshalJSON implements json.Marshaler.
func (c Cost) MarshalJSON() ([]byte, error) {
	out := costJSON{
		Completeness: c.Completeness,
		Unpriced:     c.Unpriced,
		Reason:       c.Reason,
	}
	// An amount is written whenever it means something: the total when known, the
	// floor when partial.
	if c.Completeness == CostKnown || c.Completeness == CostPartial {
		v := int64(c.Amount)
		out.Amount = &v
	}
	return json.Marshal(out)
}

// UnmarshalJSON implements json.Unmarshaler.
func (c *Cost) UnmarshalJSON(b []byte) error {
	var in costJSON
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	*c = Cost{
		Completeness: in.Completeness,
		Unpriced:     in.Unpriced,
		Reason:       in.Reason,
	}
	if in.Amount != nil {
		c.Amount = money.Money(*in.Amount)
	}
	switch c.Completeness {
	case CostKnown, CostPartial, CostUnknown:
	case "":
		// A record written before completeness existed, or one with no amount at
		// all. An amount present without a stated completeness was a known cost in
		// the older encoding; absent, it was unknown. Defaulting the amount-present
		// case to known preserves old records rather than silently downgrading real
		// spend to unpriced.
		if in.Amount != nil {
			c.Completeness = CostKnown
		} else {
			c.Completeness = CostUnknown
		}
	default:
		return fmt.Errorf("usage: unknown cost completeness %q", c.Completeness)
	}
	return nil
}
