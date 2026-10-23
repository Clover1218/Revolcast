import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath, URL } from 'node:url'

// https://vitejs.dev/config/
export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: {
    port: 5173,
    // 开发环境将 /api 代理到后端 Gin 服务，避免跨域
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
        // 同时代理 WebSocket（/api/v1/game-servers/.../ws），
        // 使前端与后端在开发期「同源」，页面手动设置的 Cookie 才能随请求送达后端。
        ws: true,
      },
    },
  },
})
