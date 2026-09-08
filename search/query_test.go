package search

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Filter parsing is combinatorial: an E2E test per combination would be wasteful, and
// a silently-dropped filter is invisible (you get results, just the wrong ones).
func TestParseFilterCombinations(t *testing.T) {
	t.Run("empty params yield an unfiltered default query", func(t *testing.T) {
		q, err := (&Params{}).parse()
		require.NoError(t, err)
		assert.Equal(t, DefaultLimit, q.Limit)
		assert.Empty(t, q.Text)
		assert.Empty(t, q.Category)
		assert.Nil(t, q.From)
		assert.Nil(t, q.To)
		assert.Nil(t, q.Location)
		assert.Nil(t, q.After)
	})

	t.Run("nil params are safe", func(t *testing.T) {
		var p *Params
		q, err := p.parse()
		require.NoError(t, err)
		assert.Equal(t, DefaultLimit, q.Limit)
	})

	t.Run("text and category are trimmed", func(t *testing.T) {
		q, err := (&Params{Q: "  jazz  ", Category: "  MUSIC "}).parse()
		require.NoError(t, err)
		assert.Equal(t, "jazz", q.Text)
		assert.Equal(t, "MUSIC", q.Category)
	})

	t.Run("whitespace-only text is treated as absent", func(t *testing.T) {
		q, err := (&Params{Q: "   "}).parse()
		require.NoError(t, err)
		assert.Empty(t, q.Text)
	})

	t.Run("all filters together", func(t *testing.T) {
		q, err := (&Params{
			Q: "jazz", Category: "MUSIC",
			From: "2026-06-01T00:00:00Z", To: "2026-07-01T00:00:00Z",
			Lat: "51.5", Lng: "-0.12", RadiusKM: "25",
			Limit: "50",
		}).parse()
		require.NoError(t, err)

		assert.Equal(t, "jazz", q.Text)
		assert.Equal(t, "MUSIC", q.Category)
		require.NotNil(t, q.From)
		require.NotNil(t, q.To)
		assert.Equal(t, 2026, q.From.Year())
		require.NotNil(t, q.Location)
		assert.InDelta(t, 51.5, q.Location.Lat, 1e-9)
		assert.InDelta(t, -0.12, q.Location.Lng, 1e-9)
		assert.InDelta(t, 25, q.Location.RadiusKM, 1e-9)
		assert.Equal(t, 50, q.Limit)
	})
}

// Zero is a legitimate coordinate. If "absent" were represented by the zero value,
// searching near the Gulf of Guinea would silently drop the location filter.
func TestParseTreatsZeroCoordinatesAsPresent(t *testing.T) {
	q, err := (&Params{Lat: "0", Lng: "0", RadiusKM: "100"}).parse()
	require.NoError(t, err)
	require.NotNil(t, q.Location, "lat=0,lng=0 is a real location, not an absent filter")
	assert.Zero(t, q.Location.Lat)
	assert.Zero(t, q.Location.Lng)
}

func TestParseLimitClamping(t *testing.T) {
	cases := map[string]int{
		"":       DefaultLimit,
		"1":      1,
		"20":     20,
		"100":    MaxLimit,
		"101":    MaxLimit,
		"100000": MaxLimit,
		// Non-positive falls back to the default rather than returning nothing.
		"0":   DefaultLimit,
		"-1":  DefaultLimit,
		"-99": DefaultLimit,
	}
	for raw, want := range cases {
		q, err := (&Params{Limit: raw}).parse()
		require.NoError(t, err, "limit=%q", raw)
		assert.Equal(t, want, q.Limit, "limit=%q", raw)
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	cases := map[string]Params{
		"non-numeric limit":     {Limit: "abc"},
		"float limit":           {Limit: "1.5"},
		"bad from":              {From: "yesterday"},
		"bad to":                {To: "2026-13-45"},
		"from without timezone": {From: "2026-06-01 00:00:00"},
		"inverted range":        {From: "2026-07-01T00:00:00Z", To: "2026-06-01T00:00:00Z"},
		"lat only":              {Lat: "51.5"},
		"lng only":              {Lng: "-0.12"},
		"radius only":           {RadiusKM: "10"},
		"lat and lng no radius": {Lat: "51.5", Lng: "-0.12"},
		"lat out of range high": {Lat: "91", Lng: "0", RadiusKM: "10"},
		"lat out of range low":  {Lat: "-91", Lng: "0", RadiusKM: "10"},
		"lng out of range high": {Lat: "0", Lng: "181", RadiusKM: "10"},
		"lng out of range low":  {Lat: "0", Lng: "-181", RadiusKM: "10"},
		"zero radius":           {Lat: "0", Lng: "0", RadiusKM: "0"},
		"negative radius":       {Lat: "0", Lng: "0", RadiusKM: "-5"},
		"absurd radius":         {Lat: "0", Lng: "0", RadiusKM: "999999"},
		"non-numeric lat":       {Lat: "north", Lng: "0", RadiusKM: "10"},
		"NaN lat":               {Lat: "NaN", Lng: "0", RadiusKM: "10"},
		"infinite radius":       {Lat: "0", Lng: "0", RadiusKM: "Inf"},
		"malformed cursor":      {Cursor: "!!!not-a-cursor"},
		"overlong query":        {Q: longString(maxQueryLen + 1)},
	}

	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := p.parse()
			assert.Error(t, err)
		})
	}
}

// Boundary coordinates are valid, not errors.
func TestParseAcceptsCoordinateBoundaries(t *testing.T) {
	cases := []Params{
		{Lat: "90", Lng: "180", RadiusKM: "1"},
		{Lat: "-90", Lng: "-180", RadiusKM: "1"},
		{Lat: "0", Lng: "0", RadiusKM: "20000"},
	}
	for _, p := range cases {
		_, err := p.parse()
		assert.NoError(t, err, "params %+v should be accepted", p)
	}
}

func TestParseAcceptsQueryAtMaxLength(t *testing.T) {
	_, err := (&Params{Q: longString(maxQueryLen)}).parse()
	assert.NoError(t, err)
}

// Timestamps are normalised to UTC so the SQL comparison is unambiguous.
func TestParseNormalisesTimeToUTC(t *testing.T) {
	q, err := (&Params{From: "2026-06-01T12:00:00+05:00"}).parse()
	require.NoError(t, err)
	require.NotNil(t, q.From)
	assert.Equal(t, time.UTC, q.From.Location())
	assert.Equal(t, 7, q.From.Hour(), "12:00+05:00 is 07:00Z")
}

func longString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
