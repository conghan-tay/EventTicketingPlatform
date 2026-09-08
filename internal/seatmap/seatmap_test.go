package seatmap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Bijective base-26 has no zero digit, so the Z→AA rollover is the classic off-by-one.
func TestRowLabel(t *testing.T) {
	cases := map[int]string{
		0:   "A",
		1:   "B",
		25:  "Z",
		26:  "AA", // rollover, not "BA"
		27:  "AB",
		51:  "AZ",
		52:  "BA",
		77:  "BZ",
		78:  "CA",
		701: "ZZ",
		702: "AAA",
	}
	for idx, want := range cases {
		assert.Equal(t, want, RowLabel(idx), "RowLabel(%d)", idx)
	}
}

func TestRowLabelsAreUniqueAcrossRollover(t *testing.T) {
	seen := make(map[string]int, 1000)
	for i := range 1000 {
		label := RowLabel(i)
		if prev, dup := seen[label]; dup {
			t.Fatalf("label %q produced by both row %d and row %d", label, prev, i)
		}
		seen[label] = i
	}
}

func TestGenerateSeatCountAndBoundaries(t *testing.T) {
	seats, err := Generate([]SectionSpec{
		{Name: "FLOOR", Rows: 10, SeatsPerRow: 20},
		{Name: "BALCONY", Rows: 2, SeatsPerRow: 5},
	})
	require.NoError(t, err)
	require.Len(t, seats, 210, "10*20 + 2*5")

	// Seats are numbered from 1, not 0 — a venue has no "seat 0".
	assert.Equal(t, SeatSpec{Section: "FLOOR", RowLabel: "A", Number: 1}, seats[0])
	assert.Equal(t, SeatSpec{Section: "FLOOR", RowLabel: "A", Number: 20}, seats[19])
	assert.Equal(t, SeatSpec{Section: "FLOOR", RowLabel: "B", Number: 1}, seats[20])
	assert.Equal(t, SeatSpec{Section: "FLOOR", RowLabel: "J", Number: 20}, seats[199])
	assert.Equal(t, SeatSpec{Section: "BALCONY", RowLabel: "A", Number: 1}, seats[200])
	assert.Equal(t, SeatSpec{Section: "BALCONY", RowLabel: "B", Number: 5}, seats[209])
}

// Every seat must be distinct, or the unique constraint on (venue, section, row, seat)
// would reject the insert at runtime.
func TestGenerateProducesNoDuplicates(t *testing.T) {
	seats, err := Generate([]SectionSpec{
		{Name: "A", Rows: 30, SeatsPerRow: 30},
		{Name: "B", Rows: 30, SeatsPerRow: 30},
	})
	require.NoError(t, err)

	seen := make(map[SeatSpec]struct{}, len(seats))
	for _, s := range seats {
		_, dup := seen[s]
		require.False(t, dup, "duplicate seat %+v", s)
		seen[s] = struct{}{}
	}
	assert.Len(t, seen, 1800)
}

func TestGenerateSingleSeatVenue(t *testing.T) {
	seats, err := Generate([]SectionSpec{{Name: "ONLY", Rows: 1, SeatsPerRow: 1}})
	require.NoError(t, err)
	assert.Equal(t, []SeatSpec{{Section: "ONLY", RowLabel: "A", Number: 1}}, seats)
}

func TestGenerateRejectsInvalidSpecs(t *testing.T) {
	cases := []struct {
		name  string
		specs []SectionSpec
	}{
		{"no sections", nil},
		{"empty slice", []SectionSpec{}},
		{"missing name", []SectionSpec{{Rows: 1, SeatsPerRow: 1}}},
		{"zero rows", []SectionSpec{{Name: "A", Rows: 0, SeatsPerRow: 5}}},
		{"negative rows", []SectionSpec{{Name: "A", Rows: -1, SeatsPerRow: 5}}},
		{"zero seats per row", []SectionSpec{{Name: "A", Rows: 5, SeatsPerRow: 0}}},
		{"duplicate section", []SectionSpec{{Name: "A", Rows: 1, SeatsPerRow: 1}, {Name: "A", Rows: 1, SeatsPerRow: 1}}},
		{"exceeds max", []SectionSpec{{Name: "A", Rows: 1000, SeatsPerRow: 1000}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Generate(tc.specs)
			assert.Error(t, err)
		})
	}
}

// The cap must consider the sum across sections, not each section alone.
func TestGenerateCapIsCumulative(t *testing.T) {
	half := MaxSeatsPerVenue / 2
	_, err := Generate([]SectionSpec{
		{Name: "A", Rows: 1, SeatsPerRow: half},
		{Name: "B", Rows: 1, SeatsPerRow: half + 10},
	})
	assert.ErrorIs(t, err, ErrTooManySeats)
}

func TestTotalSeats(t *testing.T) {
	assert.Equal(t, 210, TotalSeats([]SectionSpec{
		{Name: "FLOOR", Rows: 10, SeatsPerRow: 20},
		{Name: "BALCONY", Rows: 2, SeatsPerRow: 5},
	}))
	assert.Equal(t, 0, TotalSeats(nil))
}
