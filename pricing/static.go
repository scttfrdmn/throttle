package pricing

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/usage"
)

// Static is an in-memory catalog backed by an explicit list of prices.
//
// It is deliberately simple. v0.1 needs a correct seam more than it needs
// automatic price synchronization: adapters must not embed prices, quotes must
// carry provenance, and a user must be able to override a rate. Fetching and
// refreshing a full provider price list is separate work and does not change any
// of those properties.
//
// Safe for concurrent use.
type Static struct {
	mu sync.RWMutex

	// prices is ordered most-specific-first within each key so that a
	// region/tier-specific entry wins over a general one.
	prices map[string][]Price
}

// NewStatic builds a catalog from prices. Later entries do not overwrite earlier
// ones; all are retained and matched by specificity.
func NewStatic(prices ...Price) (*Static, error) {
	s := &Static{prices: make(map[string][]Price, len(prices))}
	for _, p := range prices {
		if err := s.Add(p); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Add inserts a price, validating it. An invalid rate is refused here rather than
// producing a wrong cost later.
func (s *Static) Add(p Price) error {
	if p.AccessProvider == "" {
		return fmt.Errorf("pricing: price has no access provider")
	}
	if p.ProviderModelID == "" {
		return fmt.Errorf("pricing: price for %s has no provider model id", p.AccessProvider)
	}
	if len(p.Rates) == 0 {
		return fmt.Errorf("pricing: price for %s has no rates", p.ProviderModelID)
	}
	for d, r := range p.Rates {
		if r.Unit <= 0 {
			return fmt.Errorf("pricing: rate for %s/%s has a non-positive unit", p.ProviderModelID, d)
		}
		if r.PerUnit < 0 {
			return fmt.Errorf("pricing: rate for %s/%s is negative", p.ProviderModelID, d)
		}
		// The map key is authoritative, so a mismatched Dimension field would be
		// a silent trap for whoever reads the rate back out.
		if r.Dimension != "" && r.Dimension != d {
			return fmt.Errorf("pricing: rate keyed %s declares dimension %s", d, r.Dimension)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(p.AccessProvider, p.ProviderModelID)
	s.prices[k] = append(s.prices[k], p)
	// Most specific first: an entry naming both region and tier outranks one
	// naming neither.
	sort.SliceStable(s.prices[k], func(i, j int) bool {
		return specificity(s.prices[k][i]) > specificity(s.prices[k][j])
	})
	return nil
}

// Override adds a price that outranks catalog entries of equal specificity,
// marking its provenance as local. This is the seam for negotiated or committed
// pricing: a user must not have to accept the shipped numbers.
func (s *Static) Override(p Price) error {
	if p.Provenance.Source == "" {
		p.Provenance.Source = "local-override"
	}
	if err := s.Add(p); err != nil {
		return err
	}
	// Overrides are prepended within their key so they win ties against
	// catalog entries that are equally specific.
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(p.AccessProvider, p.ProviderModelID)
	list := s.prices[k]
	sort.SliceStable(list, func(i, j int) bool {
		si, sj := specificity(list[i]), specificity(list[j])
		if si != sj {
			return si > sj
		}
		return isOverride(list[i]) && !isOverride(list[j])
	})
	return nil
}

func isOverride(p Price) bool { return p.Provenance.Source == "local-override" }

func key(provider, model string) string { return provider + "\x00" + model }

// specificity ranks a price by how many access dimensions it pins down.
func specificity(p Price) int {
	n := 0
	if p.Region != "" {
		n++
	}
	if p.ServiceTier != "" {
		n++
	}
	return n
}

// matches reports whether a price applies to an identity. An empty field on the
// price matches any value, so a catalog spells out only what actually varies.
func matches(p Price, id usage.ModelIdentity) bool {
	if p.Region != "" && p.Region != id.Region {
		return false
	}
	if p.ServiceTier != "" && p.ServiceTier != id.ServiceTier {
		return false
	}
	return true
}

// find selects the applicable price for an identity at a point in time.
//
// The lookup is keyed on the exact provider model ID, never on canonical
// identity, so a model throttle cannot name is still priceable as long as someone
// listed its ID.
func (s *Static) find(id usage.ModelIdentity, at time.Time) *Price {
	s.mu.RLock()
	candidates := s.prices[key(id.AccessProvider, id.ProviderModelID)]
	s.mu.RUnlock()

	for i := range candidates {
		if !matches(candidates[i], id) {
			continue
		}
		// A price that is not yet in effect must not be applied to a request
		// that happened before it.
		if ef := candidates[i].Provenance.EffectiveFrom; !ef.IsZero() && at.Before(ef) {
			continue
		}
		return &candidates[i]
	}
	return nil
}

// Capture implements RateSource: it freezes the applicable rates so settlement
// can replay them after the catalog has moved on.
//
// It also captures the model's other service tiers as alternates, in the same
// read, so that a provider serving the request on a tier the caller did not ask
// for is still priced from frozen rates rather than from a re-query.
func (s *Static) Capture(id usage.ModelIdentity, at time.Time) (CapturedQuote, error) {
	q, err := s.capture(id, at)
	if err != nil {
		return CapturedQuote{}, err
	}
	if alts := s.tiers(id, at, q.ServiceTier); len(alts) > 0 {
		q.Alternates = alts
	}
	return q, nil
}

func (s *Static) capture(id usage.ModelIdentity, at time.Time) (CapturedQuote, error) {
	price := s.find(id, at)
	if price == nil {
		reason := fmt.Sprintf("no price for %s on %s", id.ProviderModelID, id.AccessProvider)
		return CapturedQuote{}, fmt.Errorf("%w: %s", ErrNoPrice, reason)
	}

	// The rates are copied rather than referenced: a captured quote must not
	// change when the catalog it came from does, which is the whole point.
	rates := make(map[usage.Dimension]Rate, len(price.Rates))
	for d, r := range price.Rates {
		r.Dimension = d
		rates[d] = r
	}
	return CapturedQuote{
		AccessProvider:  price.AccessProvider,
		ProviderModelID: price.ProviderModelID,
		Region:          id.Region,
		// The row's tier, not the requested one. A tier-less row matches a request
		// naming any tier -- that is what "empty matches any value" on a Price means --
		// so copying the requested tier here would make the quote claim to be qualified
		// for a tier it was never priced for, and the false claim would then be
		// indistinguishable from a real tier-specific capture at settlement. See
		// CapturedQuote.ServiceTier and issue #30.
		ServiceTier: price.ServiceTier,
		Rates:       rates,
		Provenance:  price.Provenance,
		CapturedAt:  at,
	}, nil
}

// tiers captures a quote per other service tier this model is priced for, so a
// substituted tier can be priced without going back to a catalog that may have
// changed in the meantime.
func (s *Static) tiers(id usage.ModelIdentity, at time.Time, exclude string) map[string]CapturedQuote {
	s.mu.RLock()
	candidates := s.prices[key(id.AccessProvider, id.ProviderModelID)]
	named := make([]string, 0, len(candidates))
	for _, p := range candidates {
		if p.ServiceTier != "" && p.ServiceTier != exclude {
			named = append(named, p.ServiceTier)
		}
	}
	s.mu.RUnlock()

	if len(named) == 0 {
		return nil
	}
	out := make(map[string]CapturedQuote, len(named))
	for _, tier := range named {
		if _, seen := out[tier]; seen {
			continue
		}
		alt := id
		alt.ServiceTier = tier
		// Resolved through find, so tier-specific and general entries are ranked by
		// the same specificity rules that chose the primary.
		q, err := s.capture(alt, at)
		if err != nil {
			continue
		}
		// find may have fallen through to a row that is not tier-specific -- if the
		// tier's own row is not yet in effect, say. Keying that under this tier would
		// file general rates as though the tier had been priced, which is exactly the
		// mislabelling the primary capture no longer does.
		if q.ServiceTier != tier {
			continue
		}
		out[tier] = q
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Quote implements Catalog. It is Capture followed immediately by Price, for
// callers pricing usage they already have.
//
// An unknown model produces an explicit unknown cost rather than a guess or a
// zero, and usage containing a dimension the price sheet does not cover produces
// a partial cost that names what is missing.
func (s *Static) Quote(_ context.Context, id usage.ModelIdentity, u usage.Usage, at time.Time) (Quote, error) {
	q, err := s.Capture(id, at)
	if err != nil {
		reason := fmt.Sprintf("no price for %s on %s", id.ProviderModelID, id.AccessProvider)
		return Quote{Cost: usage.UnknownCost(reason)}, err
	}
	priced, err := q.Price(u)
	return Quote{
		Cost:         priced.Cost,
		PerDimension: priced.PerDimension,
		Provenance:   priced.Provenance,
		Unpriced:     priced.Unpriced,
		Captured:     q,
	}, err
}

// Models returns the priced (access provider, model ID) pairs, for diagnostics
// such as explaining why a model was unpriceable.
func (s *Static) Models() []usage.ModelIdentity {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]usage.ModelIdentity, 0, len(s.prices))
	for _, list := range s.prices {
		if len(list) == 0 {
			continue
		}
		out = append(out, usage.ModelIdentity{
			AccessProvider:  list[0].AccessProvider,
			ProviderModelID: list[0].ProviderModelID,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AccessProvider != out[j].AccessProvider {
			return out[i].AccessProvider < out[j].AccessProvider
		}
		return out[i].ProviderModelID < out[j].ProviderModelID
	})
	return out
}

// PerMillion is a convenience for the way provider prices are normally quoted.
func PerMillion(d usage.Dimension, dollarsPerMillion money.Money) Rate {
	return Rate{Dimension: d, PerUnit: dollarsPerMillion, Unit: 1_000_000}
}

// PerThousand is a convenience for older per-1K price sheets.
func PerThousand(d usage.Dimension, dollarsPerThousand money.Money) Rate {
	return Rate{Dimension: d, PerUnit: dollarsPerThousand, Unit: 1_000}
}

// PerNanoUnit prices a nano-unit dimension from the price of one whole unit: a rate
// quoted per vCPU-hour, for usage counted in billionths of a vCPU-hour.
//
// The division is the point. Dividing the price down to a per-nano-unit figure would
// lose precision or need a float; keeping the provider's price as the numerator and
// the scale as the denominator makes the arithmetic exact, exactly as PerMillion
// does for a per-million-token price.
func PerNanoUnit(d usage.Dimension, dollarsPerUnit money.Money) Rate {
	return Rate{Dimension: d, PerUnit: dollarsPerUnit, Unit: usage.NanoScale}
}
