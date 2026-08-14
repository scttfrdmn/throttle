package pricing

import (
	"sort"
	"strings"

	"github.com/scttfrdmn/throttle/usage"
)

// A selector is the set of access dimensions that choose between price sheets for
// one model.
//
// # Why this exists
//
// Issue #30 established the rule for service tier: a request served on a tier no
// rate was frozen for is unpriceable, because a tier re-rates every dimension of the
// request and the tiers that were captured bound its cost in neither direction.
//
// Service tier turned out not to be the only dimension with that property. A
// provider that prices inference geography charges a different rate for every token
// category depending on where the work happened, the geography is reported on the
// response rather than decided by the endpoint, and a workspace default means a
// request that named none can still be served in a priced one. That is #30's
// situation exactly, on a second axis.
//
// So the axes are named together rather than special-cased apart. Adding a third
// later means adding a field here and to Price, not another branch in every
// comparison.
//
// # The encoding rule
//
// key is the map key for Alternates, and for a selector with no geography it is
// byte-identical to the bare service tier that keyed alternates before geography
// existed. Quotes already persisted therefore keep resolving: a stored alternate
// under "priority" is still found by a selector naming tier priority and no
// geography. That compatibility is the reason the separator is a suffix on the tier
// rather than a joined pair of both fields.
type selector struct {
	serviceTier  string
	inferenceGeo string
}

// geoKey separates the geography from the tier in a selector key. Chosen to be
// absent from any provider's tier vocabulary, and applied only when a geography is
// present, so a tier-only key is unchanged.
const geoKey = "|geo="

func selectorOf(id usage.ModelIdentity) selector {
	return selector{serviceTier: id.ServiceTier, inferenceGeo: id.InferenceGeo}
}

func selectorOfPrice(p Price) selector {
	return selector{serviceTier: p.ServiceTier, inferenceGeo: p.InferenceGeo}
}

func (s selector) zero() bool { return s == selector{} }

func (s selector) key() string {
	if s.inferenceGeo == "" {
		return s.serviceTier
	}
	return s.serviceTier + geoKey + s.inferenceGeo
}

// covers reports whether rates qualified for s can price a request whose selector is
// want.
//
// An empty axis on want means the request says nothing about that dimension, which
// any qualification covers. A non-empty axis has to match exactly: substituting a
// rate qualified for another value is the mistake #30 exists to prevent.
//
// want must already have been narrowed by narrow, which is what blanks out an axis
// the catalog does not price at all.
func (s selector) covers(want selector) bool {
	if want.serviceTier != "" && want.serviceTier != s.serviceTier {
		return false
	}
	if want.inferenceGeo != "" && want.inferenceGeo != s.inferenceGeo {
		return false
	}
	return true
}

// describe names the axes a selector pins down, for an operator reading a reason.
func (s selector) describe() string {
	parts := make([]string, 0, 2)
	if s.serviceTier != "" {
		parts = append(parts, "service tier "+quoted(s.serviceTier))
	}
	if s.inferenceGeo != "" {
		parts = append(parts, "inference geography "+quoted(s.inferenceGeo))
	}
	if len(parts) == 0 {
		return selectorUnqualified
	}
	return strings.Join(parts, " and ")
}

func quoted(s string) string { return `"` + s + `"` }

// narrow drops the axes of want that this set of captured selectors does not price
// at all.
//
// This is the generalization of the old "these rates are not qualified by tier and no
// alternates were captured" case, and it has to be per axis rather than wholesale. A
// model priced by geography but not by tier is normal: every row names a geography and
// none names a tier. Requiring an exact match on both axes would make an ordinary
// request unpriceable because the response reported a tier the catalog never needed
// to distinguish, and requiring neither would let a real geography difference be
// priced by the wrong sheet.
//
// The captured set is the evidence. If no row this model was captured under names a
// service tier, then tier does not select between price sheets for this model and
// whatever tier served the call is covered. If some row does, the served tier has to
// be one of them.
func narrow(want selector, captured []selector) selector {
	var tierPriced, geoPriced bool
	for _, s := range captured {
		if s.serviceTier != "" {
			tierPriced = true
		}
		if s.inferenceGeo != "" {
			geoPriced = true
		}
	}
	if !tierPriced {
		want.serviceTier = ""
	}
	if !geoPriced {
		want.inferenceGeo = ""
	}
	return want
}

// selectorKeys renders a set of selectors as sorted keys, for the reason an operator
// reads. An unqualified selector is named rather than shown as an empty string: "the
// rates were not qualified" is the useful fact there.
func selectorKeys(sels []selector) []string {
	out := make([]string, 0, len(sels))
	for _, s := range sels {
		if s.zero() {
			out = append(out, selectorUnqualified)
			continue
		}
		out = append(out, s.key())
	}
	sort.Strings(out)
	return out
}
