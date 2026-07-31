import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// During dev, proxy /api calls to the Go backend so there are no CORS surprises.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5183,
    proxy: {
      '/api': 'http://127.0.0.1:8770',
    },
  },
  build: {
    // Emit directly into the backend's embed dir so `go build` ships the UI.
    outDir: '../backend/web',
    emptyOutDir: true,
  },
})
