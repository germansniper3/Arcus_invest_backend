package services

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"arcusinvest/internal/models"
)

var (
	// ErrNoLinesToApportion is returned when a receipt has no lines to spread
	// cost across. Charging freight to nothing is not a rounding question, it is
	// a missing receipt.
	ErrNoLinesToApportion = errors.New("no lines to apportion across")
	// ErrNegativeApportionment guards against a credit note being pushed through
	// the apportioner as if it were a charge.
	ErrNegativeApportionment = errors.New("cannot apportion a negative amount")
	// ErrUnknownBasis is returned for an apportionment basis that is not one of
	// AllApportionmentBases.
	ErrUnknownBasis = errors.New("unknown apportionment basis")
)

// toNgwee converts kwacha to whole ngwee, which is the unit all apportionment
// arithmetic runs in.
//
// Money is apportioned as integers, never as floats. Spreading a lump sum
// across lines in float64 leaves a residue that is invisible per line and
// material in aggregate — the sum of the parts stops equalling the whole, and
// the difference surfaces later as stock valued at a figure no invoice
// supports. Integers make the reconciliation exact by construction.
func toNgwee(kwacha float64) int64 {
	return int64(math.Round(kwacha * 100))
}

func fromNgwee(ngwee int64) float64 {
	return float64(ngwee) / 100
}

// ApportionNgwee spreads total across len(weights) buckets in proportion to
// weights, and guarantees that the result sums back to total exactly.
//
// The method is largest-remainder: give every bucket its whole-ngwee floor
// share, then hand the undistributed remainder out one ngwee at a time to the
// buckets with the largest fractional parts. Ties go to the earlier bucket, so
// the same inputs always produce the same answer — an apportionment that varies
// between runs cannot be reconciled to the ledger it wrote.
//
// Where every weight is zero — apportioning by weight on a receipt where nobody
// keyed the weights — the charge is split as evenly as the remainder allows
// rather than failing. A cost that arrived is still a cost; refusing to place
// it would leave it out of stock valuation entirely, which is the worse error.
func ApportionNgwee(total int64, weights []int64) ([]int64, error) {
	if len(weights) == 0 {
		return nil, ErrNoLinesToApportion
	}
	if total < 0 {
		return nil, ErrNegativeApportionment
	}

	out := make([]int64, len(weights))
	if total == 0 {
		return out, nil
	}

	var totalWeight int64
	for _, w := range weights {
		if w < 0 {
			return nil, fmt.Errorf("%w: negative weight", ErrNegativeApportionment)
		}
		totalWeight += w
	}

	// No weight to go on: fall back to an even split. Documented above.
	if totalWeight == 0 {
		for i := range weights {
			weights[i] = 1
		}
		totalWeight = int64(len(weights))
	}

	// Floor share, and the remainder each bucket was owed but did not get.
	type slot struct {
		index     int
		remainder int64
	}
	slots := make([]slot, len(weights))
	var placed int64
	for i, w := range weights {
		exact := total * w           // scaled by totalWeight
		share := exact / totalWeight // floor
		out[i] = share
		placed += share
		slots[i] = slot{index: i, remainder: exact % totalWeight}
	}

	// Hand out what floor division left behind: largest remainder first, and on
	// a tie the earlier line. SliceStable keeps that tie-break deterministic.
	left := total - placed
	sort.SliceStable(slots, func(a, b int) bool {
		return slots[a].remainder > slots[b].remainder
	})
	for i := int64(0); i < left && int(i) < len(slots); i++ {
		out[slots[i].index]++
	}

	return out, nil
}

// LineBasisWeight is the quantity a single receipt line contributes to the
// apportionment, in the units the basis calls for.
type LineBasisWeight struct {
	// ValueNgwee is the line's supplier value in kwacha ngwee at the receipt's
	// exchange rate.
	ValueNgwee int64
	// Quantity is the units received on the line.
	Quantity int64
	// WeightGrams is the line's total weight, in grams so the basis stays
	// integral.
	WeightGrams int64
}

// basisWeights picks the weight vector for a basis.
func basisWeights(basis string, lines []LineBasisWeight) ([]int64, error) {
	out := make([]int64, len(lines))
	for i, l := range lines {
		switch basis {
		case models.BasisValue:
			out[i] = l.ValueNgwee
		case models.BasisQuantity:
			out[i] = l.Quantity
		case models.BasisWeight:
			out[i] = l.WeightGrams
		default:
			return nil, fmt.Errorf("%w: %q", ErrUnknownBasis, basis)
		}
	}
	return out, nil
}

// ApportionReceipt spreads componentsZMW across lines on the given basis and
// returns each line's share, in kwacha.
//
// The returned shares sum to componentsZMW exactly, to the ngwee. That property
// is the whole point of the function and is what the tests assert.
func ApportionReceipt(basis string, componentsZMW float64, lines []LineBasisWeight) ([]float64, error) {
	weights, err := basisWeights(basis, lines)
	if err != nil {
		return nil, err
	}
	shares, err := ApportionNgwee(toNgwee(componentsZMW), weights)
	if err != nil {
		return nil, err
	}
	out := make([]float64, len(shares))
	for i, s := range shares {
		out[i] = fromNgwee(s)
	}
	return out, nil
}
