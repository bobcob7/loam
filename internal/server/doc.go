// Package server builds the single HTTP dispatch point described in
// docs/web-spec.md -> Hosting & Routing: one mux, one port, four path
// groups (loam-ofg.2's "LISTENER and DISPATCH" half of the composition
// root; loam-ofg.20/.21/.22 own config loading, the rest of the startup
// sequence, and the health-check handler bodies respectively).
//
// Router owns exactly the routing decision, not the handlers themselves:
//   - /loam.v1.* (CLI Connect services) behind httpauth.Auth.CLI
//   - /loam.admin.v1.* (admin Connect services) behind httpauth.Auth.AdminOnly
//   - /git/* (smart HTTP) behind httpauth.Auth.GitIdentity
//   - /healthz, /readyz registered with NO auth wrapper at all — the
//     exemption is a routing fact, not a special case inside any
//     middleware (docs/server-spec.md: "the only such exemption";
//     docs/web-spec.md -> Auth, Health)
//   - everything else: the embedded SPA, behind AdminOnly, falling back to
//     index.html for unknown non-API paths
//
// Each RegisterXxx method is the plug-in point later handler beads use —
// one call per Connect service constructor, added to cmd/server/main.go —
// so there is no serialized final integration bead
// (docs/bead-workflow.md's per-bead worktree model; see the DESIGN note on
// loam-ofg.2).
package server
