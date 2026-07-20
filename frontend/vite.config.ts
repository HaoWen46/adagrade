import { defineConfig } from "vite";
import react from "@vitejs/plugin-react-swc";
import tailwindcss from "@tailwindcss/vite";

// Build output goes into the Go binary's embed tree (docs/DECISIONS.md D9):
// internal/web/assets/dist is gitignored and picked up by go:embed when present.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    outDir: "../internal/web/assets/dist",
    emptyOutDir: true,
  },
  server: {
    proxy: {
      "/api": "http://localhost:8080",
      "/auth": "http://localhost:8080",
      "/healthz": "http://localhost:8080",
      "/readyz": "http://localhost:8080",
    },
  },
});
