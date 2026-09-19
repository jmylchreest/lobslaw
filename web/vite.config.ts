import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The build output is embedded into the Go binary by
// internal/gateway/ui, so it is written straight there rather than to
// a local dist/ that a Makefile step would then have to copy. One
// artefact, one location, and nothing to forget.
//
// base is ABSOLUTE. It was "./" for a reverse proxy at a subpath, and
// that broke every deep link: on /bots/engineering the browser
// resolves "./assets/x.js" to /bots/assets/x.js, the SPA fallback
// answers it with index.html, and the module fails MIME checking —
// a blank white page with one console line.
//
// The integration test passed throughout, because it only checked the
// SHELL came back 200 and never loaded the bundle.
export default defineConfig({
  plugins: [react()],
  base: "/",
  build: {
    outDir: "../internal/gateway/ui/dist",
    // NOT emptied by Vite. That directory holds a committed .gitkeep,
    // and without it a clean checkout has no dist/ at all — which
    // makes `go:embed all:dist` a COMPILE error for anyone who has not
    // run the web build. `make web` removes the hashed assets itself,
    // which is the only part that would otherwise accumulate.
    emptyOutDir: false,
  },
  server: {
    // Dev server talks to a locally running node, so the console can
    // be worked on without rebuilding the binary on every change.
    proxy: {
      "/v1": "http://127.0.0.1:8080",
    },
  },
  test: {
    environment: "node",
  },
});
