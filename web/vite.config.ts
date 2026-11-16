import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  // Caddy (in Docker) reaches the dev server at host.docker.internal:5173, which
  // only works for an IPv4 loopback listener, not Vite's default of ::1 (ADR 014).
  server: { host: '127.0.0.1', port: 5173, strictPort: true },
})
