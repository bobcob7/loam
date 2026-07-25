// Package httpauth implements the two auth regimes described in
// docs/web-spec.md -> Auth: admin HTTP basic auth and trusted agent
// identity headers. It provides composable http.Handler wrappers and the
// context accessors handlers use to read the resolved caller; loam-ofg.2
// wires the wrappers onto the actual path groups (this package does not
// know about the mux).
package httpauth

import "context"

// Identity is the caller resolved from the Loam-Agent-* request headers
// (docs/cli-spec.md -> Environment Variables), trusted as-is in the MVP: no
// signature or credential backs these values.
type Identity struct {
	Name string
	ID   string
	Role string
}

// Identifier renders the identity in the "<name>-<id>-<role>" form used by
// the CLI's `whoami` command (docs/cli-spec.md).
func (i Identity) Identifier() string {
	return i.Name + "-" + i.ID + "-" + i.Role
}

type contextKey int

const (
	identityContextKey contextKey = iota
	adminContextKey
)

// WithIdentity returns a copy of ctx carrying the resolved agent identity.
func WithIdentity(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, identityContextKey, identity)
}

// IdentityFromContext returns the agent identity carried by ctx, if any.
// ok is false when the request carried no (or incomplete) agent identity
// headers — including every request on a path group the admin reached as
// superuser, since AdminOnly and the admin branch of CLI never set one.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(identityContextKey).(Identity)
	return identity, ok
}

// WithAdmin returns a copy of ctx marked as an authenticated admin
// superuser (docs/web-spec.md: "The admin is a superuser").
func WithAdmin(ctx context.Context) context.Context {
	return context.WithValue(ctx, adminContextKey, true)
}

// IsAdmin reports whether ctx was marked as an authenticated admin
// superuser by AdminOnly or the admin branch of CLI.
func IsAdmin(ctx context.Context) bool {
	admin, _ := ctx.Value(adminContextKey).(bool)
	return admin
}

// withoutAdmin returns a copy of ctx explicitly marked as not an admin
// superuser, overriding any WithAdmin set by an ancestor context. Used by
// GitIdentity so /git/* can never inherit admin status regardless of how
// a future mux nests these wrappers.
func withoutAdmin(ctx context.Context) context.Context {
	return context.WithValue(ctx, adminContextKey, false)
}
