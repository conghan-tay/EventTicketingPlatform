//go:build e2e

package e2e

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// H1: the app boots and reports healthy.
func TestHealth(t *testing.T) {
	c := NewClient()
	require.NoError(t, c.WaitForHealth(healthTimeout))

	resp, err := c.Get("/health", nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.Status, "body: %s", resp.Body)

	var got struct {
		Status  string `json:"status"`
		Service string `json:"service"`
	}
	require.NoError(t, resp.DecodeInto(&got))
	assert.Equal(t, "ok", got.Status)
	assert.Equal(t, "eventticketing", got.Service)
}
