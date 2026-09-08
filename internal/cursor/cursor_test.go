package cursor

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRoundTrip(t *testing.T) {
	want := Key{
		StartsAt: time.Date(2026, 7, 4, 19, 30, 0, 0, time.UTC),
		EventID:  4242,
	}

	enc, err := Encode(want)
	require.NoError(t, err)
	require.NotEmpty(t, enc)

	got, err := Decode(enc)
	require.NoError(t, err)
	assert.Equal(t, want.EventID, got.EventID)
	assert.True(t, want.StartsAt.Equal(got.StartsAt), "want %s got %s", want.StartsAt, got.StartsAt)
}

// The cursor travels in a query string, so it must survive URL encoding untouched.
func TestEncodeIsURLSafe(t *testing.T) {
	enc, err := Encode(Key{StartsAt: time.Now().UTC(), EventID: 1})
	require.NoError(t, err)

	for _, c := range enc {
		assert.NotContains(t, "+/=?&#%", string(c),
			"cursor must be URL-safe, found %q in %q", string(c), enc)
	}
}

// Sub-second precision must survive, or two events starting in the same second could
// be skipped or repeated across a page boundary.
func TestRoundTripPreservesSubSecondPrecision(t *testing.T) {
	want := Key{
		StartsAt: time.Date(2026, 7, 4, 19, 30, 12, 123456789, time.UTC),
		EventID:  7,
	}
	enc, err := Encode(want)
	require.NoError(t, err)

	got, err := Decode(enc)
	require.NoError(t, err)
	assert.True(t, want.StartsAt.Equal(got.StartsAt),
		"lost precision: want %s got %s", want.StartsAt.Format(time.RFC3339Nano), got.StartsAt.Format(time.RFC3339Nano))
}

// A malformed cursor is a client error. If any of these produced a zero Key without an
// error, the query would silently restart from the beginning and repeat results.
func TestDecodeRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"empty":              "",
		"not base64":         "!!!not-base64!!!",
		"base64 but garbage": base64.RawURLEncoding.EncodeToString([]byte("hello")),
		"empty json":         base64.RawURLEncoding.EncodeToString([]byte(`{}`)),
		"json array":         base64.RawURLEncoding.EncodeToString([]byte(`[]`)),
		"missing event id":   base64.RawURLEncoding.EncodeToString([]byte(`{"s":"2026-07-04T19:30:00Z"}`)),
		"zero event id":      base64.RawURLEncoding.EncodeToString([]byte(`{"s":"2026-07-04T19:30:00Z","e":0}`)),
		"negative event id":  base64.RawURLEncoding.EncodeToString([]byte(`{"s":"2026-07-04T19:30:00Z","e":-5}`)),
		"missing start":      base64.RawURLEncoding.EncodeToString([]byte(`{"e":5}`)),
		"bad time format":    base64.RawURLEncoding.EncodeToString([]byte(`{"s":"not-a-time","e":5}`)),
		"unknown field":      base64.RawURLEncoding.EncodeToString([]byte(`{"s":"2026-07-04T19:30:00Z","e":5,"x":1}`)),
		"truncated":          "eyJzIjoi",
	}

	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Decode(in)
			require.Error(t, err, "input %q should be rejected", in)
			assert.ErrorIs(t, err, ErrMalformed)
		})
	}
}

func TestEncodeRejectsInvalidKey(t *testing.T) {
	for _, id := range []int64{0, -1} {
		_, err := Encode(Key{StartsAt: time.Now().UTC(), EventID: id})
		assert.ErrorIs(t, err, ErrMalformed, "event id %d", id)
	}
}
