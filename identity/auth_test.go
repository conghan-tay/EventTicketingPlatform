package identity

import (
	"testing"

	"encore.dev"
	"github.com/stretchr/testify/assert"
)

// The development auth handler accepts an unverified bearer token as an identity, so
// the environment guard is the only thing preventing that from ever being live.
// It must fail closed.
func TestProductionLike(t *testing.T) {
	cases := []struct {
		name    string
		envType encore.EnvironmentType
		cloud   encore.CloudProvider
		want    bool
	}{
		{"unit test", encore.EnvTest, encore.CloudLocal, false},
		{"local run", encore.EnvDevelopment, encore.CloudLocal, false},
		{"cloud-hosted development", encore.EnvDevelopment, encore.CloudAWS, true},
		{"production", encore.EnvProduction, encore.CloudAWS, true},
		{"ephemeral preview", encore.EnvEphemeral, encore.CloudGCP, true},
		{"unknown future env type fails closed", encore.EnvironmentType("something-new"), encore.CloudLocal, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ProductionLike(tc.envType, tc.cloud))
		})
	}
}

func TestUserIDPattern(t *testing.T) {
	valid := []string{"alice", "user_1", "user-1", "ABC123", "aaa"}
	for _, v := range valid {
		assert.True(t, userIDPattern.MatchString(v), "expected %q to be valid", v)
	}

	invalid := []string{
		"",
		"ab", // too short
		"has space",
		"drop';--", // would be nasty in a log or query
		"unicode·dot",
		"a/../b",
	}
	for _, v := range invalid {
		assert.False(t, userIDPattern.MatchString(v), "expected %q to be rejected", v)
	}

	long := ""
	for range 65 {
		long += "a"
	}
	assert.False(t, userIDPattern.MatchString(long), "65 characters must be rejected")
}
