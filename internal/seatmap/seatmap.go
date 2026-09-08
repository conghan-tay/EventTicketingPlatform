// Package seatmap generates a venue's seat topology.
//
// This is a pure function deliberately kept out of the organizer service: an
// off-by-one in row or seat numbering is silent (you get a plausible-looking venue
// with the wrong capacity), and it is far cheaper to pin down with a table test than
// through an E2E assertion on counts.
package seatmap

import (
	"errors"
	"fmt"
)

// MaxSeatsPerVenue bounds a single venue definition. Real venues top out around
// 100k seats; the limit exists so a typo cannot try to materialise millions of rows.
const MaxSeatsPerVenue = 200_000

// SectionSpec describes a rectangular block of seats.
type SectionSpec struct {
	Name        string `json:"name"`
	Rows        int    `json:"rows"`
	SeatsPerRow int    `json:"seats_per_row"`
}

// SeatSpec is a single generated seat.
type SeatSpec struct {
	Section  string `json:"section"`
	RowLabel string `json:"row_label"`
	Number   int    `json:"seat_number"`
}

var (
	ErrNoSections    = errors.New("at least one section is required")
	ErrTooManySeats  = errors.New("venue exceeds the maximum supported seat count")
	ErrDuplicateName = errors.New("duplicate section name")
)

// Generate expands section specs into individual seats.
//
// Rows are labelled A, B, ... Z, AA, AB, ... and seats are numbered from 1, matching
// how venues actually label them.
func Generate(specs []SectionSpec) ([]SeatSpec, error) {
	if len(specs) == 0 {
		return nil, ErrNoSections
	}

	total := 0
	seen := make(map[string]struct{}, len(specs))
	for _, s := range specs {
		if s.Name == "" {
			return nil, errors.New("section name is required")
		}
		if _, dup := seen[s.Name]; dup {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateName, s.Name)
		}
		seen[s.Name] = struct{}{}

		if s.Rows <= 0 {
			return nil, fmt.Errorf("section %q: rows must be positive", s.Name)
		}
		if s.SeatsPerRow <= 0 {
			return nil, fmt.Errorf("section %q: seats_per_row must be positive", s.Name)
		}

		total += s.Rows * s.SeatsPerRow
		if total > MaxSeatsPerVenue {
			return nil, fmt.Errorf("%w: %d > %d", ErrTooManySeats, total, MaxSeatsPerVenue)
		}
	}

	seats := make([]SeatSpec, 0, total)
	for _, s := range specs {
		for r := range s.Rows {
			label := RowLabel(r)
			for n := 1; n <= s.SeatsPerRow; n++ {
				seats = append(seats, SeatSpec{Section: s.Name, RowLabel: label, Number: n})
			}
		}
	}
	return seats, nil
}

// RowLabel converts a zero-based row index into a spreadsheet-style label:
// 0→A, 25→Z, 26→AA, 27→AB, 702→AAA.
//
// Note this is bijective base-26, not plain base-26: there is no "zero digit", so
// each position runs A–Z with no placeholder. Getting this wrong is the classic
// off-by-one here, which is why it is tested directly.
func RowLabel(i int) string {
	if i < 0 {
		return ""
	}
	var out []byte
	for {
		out = append([]byte{byte('A' + i%26)}, out...)
		i = i/26 - 1
		if i < 0 {
			break
		}
	}
	return string(out)
}

// TotalSeats reports how many seats the specs describe, without allocating them.
func TotalSeats(specs []SectionSpec) int {
	total := 0
	for _, s := range specs {
		total += s.Rows * s.SeatsPerRow
	}
	return total
}
