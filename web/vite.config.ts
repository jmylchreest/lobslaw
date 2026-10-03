import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { VitePWA } from "vite-plugin-pwa";
import { rmSync } from "node:fs";

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
  plugins: [{
    name: "clean-console-assets",
    apply: "build",
    buildStart() {
      // Preserve dist/.gitkeep, but never precache obsolete bundles from a
      // previous build (npm run build is also used outside make web).
      rmSync(new URL("../internal/gateway/ui/dist/assets", import.meta.url), { recursive: true, force: true });
    },
  }, react(), VitePWA({
    registerType: "prompt",
    injectRegister: false,
    manifest: {
      id: "/",
      name: "Lobslaw",
      short_name: "Lobslaw",
      description: "Your agents, conversations, tasks and approvals.",
      start_url: "/",
      scope: "/",
      display: "standalone",
      background_color: "#000000",
      theme_color: "#000000",
      icons: [
        { src: "/pwa-192.png", sizes: "192x192", type: "image/png", purpose: "any" },
        { src: "/logo-512.png", sizes: "512x512", type: "image/png", purpose: "any" },
        { src: "/pwa-maskable-512.png", sizes: "512x512", type: "image/png", purpose: "maskable" },
      ],
    },
    workbox: {
      importScripts: ["/push-worker.js"],
      // Only the public app shell is cached. Chats, credentials, API responses
      // and mutations always go to the server; nothing is queued for replay.
      globPatterns: ["**/*.{js,css,html,png,ico,webmanifest}"],
      navigateFallback: "/index.html",
      navigateFallbackDenylist: [/^\/v1(?:\/|$)/, /^\/(?:healthz|readyz)(?:\/|$)/],
      cleanupOutdatedCaches: true,
    },
  })],
  base: "/",
  build: {
    outDir: "../internal/gateway/ui/dist",
    // NOT emptied by Vite. That directory holds a committed .gitkeep,
    // and without it a clean checkout has no dist/ at all — which
    // makes `go:embed all:dist` a COMPILE error for anyone who has not
    // run the web build. clean-console-assets removes hashed assets.
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
