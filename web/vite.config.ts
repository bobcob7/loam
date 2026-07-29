import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

// The dev proxy target: a locally running Loam server
// (docs/web-frontend-spec.md -> Dev Workflow). Overridable so a developer
// running the server on another port does not have to edit this file.
const devServerTarget = process.env["LOAM_DEV_SERVER_URL"] ?? "http://localhost:8080";

// The Connect path prefixes the SPA calls (docs/web-spec.md -> Hosting &
// Routing). Connect RPC paths are `/<fully.qualified.Service>/<Method>`, so a
// prefix match on the proto package plus its trailing dot routes every RPC in
// that package. Both are proxied: the admin is a superuser and the SPA mixes
// `loam.admin.v1` calls with `loam.v1` WorkBranchService calls.
const rpcPathPrefixes = ["/loam.v1.", "/loam.admin.v1."] as const;

const rpcProxy = Object.fromEntries(
  rpcPathPrefixes.map((prefix) => [prefix, { target: devServerTarget, changeOrigin: true }]),
);

// A default export is Vite's required config contract, and the one deliberate
// exception to the repo's named-exports-only rule (CLAUDE.md); application
// code under src/ uses named exports throughout.
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: rpcProxy,
  },
  build: {
    // web/dist is what web/embed.go embeds via `//go:embed all:dist`; the
    // path is load-bearing for the Go build, not a preference.
    outDir: "dist",
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: "./src/test/setup.ts",
  },
});
