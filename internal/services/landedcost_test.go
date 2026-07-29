package services

import (
	"errors"
	"testing"

	"arcusinvest/internal/models"
)

// The property that matters more than any individual figure: whatever the
// apportionment does with the remainder, the parts must add back to the whole.
// A freight bill that half-lands leaves stock carried at a value no invoice
// supports, and the discrepancy only ever surfaces at a stock take.
func TestApportionmentSumsBackToTheCostIncurred(t *testing.T) {
	cases := []struct {
		name    string
		total   int64
		weights []int64
	}{
		{"divides evenly", 30000, []int64{1, 1, 1}},
		{"one ngwee left over", 10000, []int64{1, 1, 1}},
		{"two ngwee left over", 20000, []int64{1, 1, 1}},
		{"lumpy weights", 123457, []int64{7, 11, 13, 17}},
		{"one line takes everything", 99999, []int64{1}},
		{"a line with no weight gets nothing", 50000, []int64{0, 1, 1}},
		{"all weights zero falls back to an even split", 10000, []int64{0, 0, 0}},
		{"prime total across prime lines", 999983, []int64{3, 5, 7, 11, 13}},
		{"very small total, many lines", 3, []int64{1, 1, 1, 1, 1}},
		{"zero total", 0, []int64{5, 5}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ApportionNgwee(tc.total, append([]int64(nil), tc.weights...))
			if err != nil {
				t.Fatalf("ApportionNgwee: %v", err)
			}
			var sum int64
			for _, g := range got {
				if g < 0 {
					t.Errorf("negative share in %v", got)
				}
				sum += g
			}
			if sum != tc.total {
				t.Errorf("shares %v sum to %d, want %d", got, sum, tc.total)
			}
			if len(got) != len(tc.weights) {
				t.Errorf("got %d shares for %d lines", len(got), len(tc.weights))
			}
		})
	}
}

// A zero-weight line must not absorb cost when other lines carry weight — that
// is the difference between "we received none of this" and "this one was free".
func TestZeroWeightLineTakesNoShare(t *testing.T) {
	got, err := ApportionNgwee(50000, []int64{0, 1, 1})
	if err != nil {
		t.Fatalf("ApportionNgwee: %v", err)
	}
	if got[0] != 0 {
		t.Errorf("zero-weight line took %d ngwee, want 0", got[0])
	}
	if got[1]+got[2] != 50000 {
		t.Errorf("weighted lines took %d, want the whole 50000", got[1]+got[2])
	}
}

// The remainder must land somewhere predictable. Largest remainder first, and
// on a tie the earlier line — so the same inputs always write the same ledger.
func TestRemainderGoesToTheLargestRemainderThenTheEarlierLine(t *testing.T) {
	// 100 ngwee across three equal lines: 33 each, 1 over. All remainders tie,
	// so the first line takes it.
	got, err := ApportionNgwee(100, []int64{1, 1, 1})
	if err != nil {
		t.Fatalf("ApportionNgwee: %v", err)
	}
	want := []int64{34, 33, 33}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}

	// Same inputs, run again: identical answer.
	again, err := ApportionNgwee(100, []int64{1, 1, 1})
	if err != nil {
		t.Fatalf("ApportionNgwee: %v", err)
	}
	for i := range again {
		if again[i] != got[i] {
			t.Fatalf("apportionment is not deterministic: %v then %v", got, again)
		}
	}
}

// Apportioning by value is the default and must load cost onto the expensive
// line, not spread it evenly.
func TestValueBasisLoadsCostByLineValue(t *testing.T) {
	lines := []LineBasisWeight{
		{ValueNgwee: 100000, Quantity: 1, WeightGrams: 50000}, // K1,000, heavy
		{ValueNgwee: 900000, Quantity: 1, WeightGrams: 1000},  // K9,000, light
	}
	shares, err := ApportionReceipt(models.BasisValue, 1000, lines)
	if err != nil {
		t.Fatalf("ApportionReceipt: %v", err)
	}
	if shares[0] != 100 || shares[1] != 900 {
		t.Errorf("value basis gave %v, want [100 900]", shares)
	}
}

// Weight is the honest basis for freight on a mixed consignment, and it must
// produce a different answer from value or there was no point offering it.
func TestWeightBasisLoadsCostByWeightNotValue(t *testing.T) {
	lines := []LineBasisWeight{
		{ValueNgwee: 100000, Quantity: 1, WeightGrams: 9000}, // cheap but heavy
		{ValueNgwee: 900000, Quantity: 1, WeightGrams: 1000}, // dear but light
	}
	shares, err := ApportionReceipt(models.BasisWeight, 1000, lines)
	if err != nil {
		t.Fatalf("ApportionReceipt: %v", err)
	}
	if shares[0] != 900 || shares[1] != 100 {
		t.Errorf("weight basis gave %v, want [900 100] — the heavy line carries the freight", shares)
	}
}

func TestQuantityBasisSplitsByUnitCount(t *testing.T) {
	lines := []LineBasisWeight{
		{ValueNgwee: 100000, Quantity: 3},
		{ValueNgwee: 900000, Quantity: 1},
	}
	shares, err := ApportionReceipt(models.BasisQuantity, 400, lines)
	if err != nil {
		t.Fatalf("ApportionReceipt: %v", err)
	}
	if shares[0] != 300 || shares[1] != 100 {
		t.Errorf("quantity basis gave %v, want [300 100]", shares)
	}
}

// ApportionReceipt works in kwacha at the boundary but must not lose ngwee in
// the float conversion on the way in or out.
func TestApportionReceiptSumsBackInKwacha(t *testing.T) {
	lines := []LineBasisWeight{
		{ValueNgwee: 33333, Quantity: 1},
		{ValueNgwee: 33333, Quantity: 1},
		{ValueNgwee: 33334, Quantity: 1},
	}
	const total = 1234.57
	shares, err := ApportionReceipt(models.BasisValue, total, lines)
	if err != nil {
		t.Fatalf("ApportionReceipt: %v", err)
	}
	var sum int64
	for _, s := range shares {
		sum += toNgwee(s)
	}
	if sum != toNgwee(total) {
		t.Errorf("shares %v sum to %d ngwee, want %d", shares, sum, toNgwee(total))
	}
}

func TestApportionmentRejectsBadInput(t *testing.T) {
	if _, err := ApportionNgwee(100, nil); !errors.Is(err, ErrNoLinesToApportion) {
		t.Errorf("no lines: got %v, want ErrNoLinesToApportion", err)
	}
	if _, err := ApportionNgwee(-1, []int64{1}); !errors.Is(err, ErrNegativeApportionment) {
		t.Errorf("negative total: got %v, want ErrNegativeApportionment", err)
	}
	if _, err := ApportionNgwee(100, []int64{1, -1}); !errors.Is(err, ErrNegativeApportionment) {
		t.Errorf("negative weight: got %v, want ErrNegativeApportionment", err)
	}
	if _, err := ApportionReceipt("by_vibes", 100, []LineBasisWeight{{Quantity: 1}}); !errors.Is(err, ErrUnknownBasis) {
		t.Errorf("unknown basis: got %v, want ErrUnknownBasis", err)
	}
}
