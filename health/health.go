// Package health exposes a liveness endpoint used by the E2E harness to wait for
// the app to finish booting before running tests.
package health

import "context"

type Status struct {
	Status  string `json:"status"`
	Service string `json:"service"`
}

//encore:api public method=GET path=/health
func Health(ctx context.Context) (*Status, error) {
	return &Status{Status: "ok", Service: "eventticketing"}, nil
}
