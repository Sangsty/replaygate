import react from '@vitejs/plugin-react';
import { defineConfig } from 'vitest/config';

// In development the dashboard runs on :5173 and forwards API calls to the Go
// server, so the browser sees one origin: no CORS, and the session cookie works.
const api = process.env.VITE_API_PROXY ?? 'http://localhost:8080';

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: { '/v1': api, '/auth': api },
  },
  test: { environment: 'node', include: ['src/**/*.test.ts'] },
});
