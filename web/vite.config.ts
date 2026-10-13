import { defineConfig } from 'vite';

// In dev, Vite serves the UI and proxies data requests to the Go server.
const backend = 'http://127.0.0.1:8080';

export default defineConfig({
  // The lazily loaded viewer chunk is mostly three.js; that's expected.
  build: { chunkSizeWarningLimit: 700 },
  server: {
    proxy: {
      '/api': backend,
      '/thumbs': backend,
    },
  },
});
