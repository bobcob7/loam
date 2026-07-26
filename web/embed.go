// Package web embeds the admin single-page app's build output
// (docs/web-frontend-spec.md -> Build & Embed: "Go embeds web/dist ... via
// //go:embed") for internal/server to serve behind admin basic auth
// (docs/web-spec.md -> Hosting & Routing).
//
// This file has to live inside web/ itself, not under internal/ where the
// rest of the server's Go code lives: a //go:embed pattern is resolved
// relative to its own source file and may not contain ".." path elements,
// so the only way to embed web/dist is from a source file that is a
// sibling of dist/ (see https://pkg.go.dev/embed). web/ is otherwise an
// npm project (docs/web-frontend-spec.md -> Project Layout); this file is
// the one deliberate exception, kept to the embed directive and a single
// accessor.
//
// dist/ ships a committed placeholder index.html (and a placeholder file
// under assets/) rather than being empty or gitignored for now, because
// go:embed treats a missing embed target as a hard build error — the same
// constraint loam-54o.2 solved for an empty migrations directory by
// shipping a real seed migration instead of a .gitkeep. Once the frontend
// epic (loam-nvb) starts producing real `task web:build` output to this
// same path, it overwrites the placeholder; the embed directive and the
// path do not change.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// Dist returns the embedded SPA build output rooted at index.html (the
// "dist" embed prefix trimmed off), ready to serve with an fs.FS-based
// file server plus SPA fallback.
func Dist() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// Unreachable: "dist" is a literal, compile-time-verified embed
		// path, not runtime input.
		panic("web: sub-filesystem of embedded dist: " + err.Error())
	}
	return sub
}
