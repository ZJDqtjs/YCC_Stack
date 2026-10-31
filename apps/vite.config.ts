import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { fileURLToPath, URL } from 'node:url'

// The broker the dev server proxies to. Point it at a local server with
// STARSTACK_BROKER=http://127.0.0.1:8787 when you want to test offline.
const broker = process.env.STARSTACK_BROKER || 'http://20.24.210.119:8787'

export default defineConfig({
  root: fileURLToPath(new URL('./mobile', import.meta.url)),
  base: './',
  plugins: [react()],
  build: { outDir: 'dist', emptyOutDir: true, chunkSizeWarningLimit: 900 },
  server: {
    host: true,
    port: 5173,
    proxy: { '/api': { target: broker, changeOrigin: true } },
  },
})
