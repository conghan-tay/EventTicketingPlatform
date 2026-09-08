// Package identity resolves the caller's identity for endpoints declared with the
// `auth` access level.
//
// SECURITY NOTE (decision D14): this is a development-grade auth handler. It treats
// the bearer token as the user id after format validation. That is acceptable for
// local development and the test suite, and it is deliberately hard-failed in
// production so it cannot be deployed by accident. Replace it with real token
// verification (JWT signature or an identity provider lookup) before any real
// deployment.
//
// The important property it *does* establish correctly, and which the rest of the
// system depends on, is that identity always arrives via the verified auth context
// and is never read from a request body.
package identity

import (
	"context"
	"regexp"

	"encore.dev"
	"encore.dev/beta/auth"
	"encore.dev/beta/errs"
)

type Data struct {
	UserID string `json:"user_id"`
}

// userIDPattern keeps ids to a predictable shape so a token cannot smuggle odd
// characters into logs, cache keys or downstream queries.
var userIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{3,64}$`)

//encore:authhandler
func AuthHandler(ctx context.Context, token string) (auth.UID, *Data, error) {
	if isProductionLike() {
		// Fail closed rather than accept unverified identity in a real environment.
		return "", nil, &errs.Error{
			Code:    errs.Unimplemented,
			Message: "development auth handler is not permitted in this environment",
		}
	}

	if !userIDPattern.MatchString(token) {
		return "", nil, &errs.Error{
			Code:    errs.Unauthenticated,
			Message: "invalid credentials",
		}
	}

	return auth.UID(token), &Data{UserID: token}, nil
}

// isProductionLike reports whether this environment must not accept development
// credentials.
func isProductionLike() bool {
	env := encore.Meta().Environment
	return ProductionLike(env.Type, env.Cloud)
}

// ProductionLike is the pure decision behind isProductionLike, separated so it can be
// unit-tested across the full environment matrix.
//
// It is the exact complement of clock.TimeControllable: anything other than a unit
// test or a locally-running app is treated as production-like and refuses development
// credentials. Deliberately defaults to "yes, production-like" so that an
// environment type added by a future Encore release fails closed.
func ProductionLike(envType encore.EnvironmentType, cloud encore.CloudProvider) bool {
	switch envType {
	case encore.EnvTest:
		return false
	case encore.EnvDevelopment:
		return cloud != encore.CloudLocal
	default:
		return true
	}
}
