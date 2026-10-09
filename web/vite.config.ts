import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

export default defineConfig({
  plugins: [react()],
  build: {
    outDir: '../internal/webui/static',
    emptyOutDir: true,
    assetsDir: 'assets',
    sourcemap: false,
  },
  server: {
    proxy: {
      '/api': { target: process.env.WWAN_API_URL || 'http://127.0.0.1:9090', ws: true },
    },
  },
});
