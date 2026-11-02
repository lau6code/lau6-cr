import { fileURLToPath, URL } from 'node:url'

import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vite'

// 后端把 dist/ 挂在站点根路径，因此 base 必须是 '/'
//
// ⚠️ 代理目标是后端**本机监听端口**，不是对外发布端口。
//
//	后端本地跑（go run / GoLand）监听 CR_LISTEN_ADDR，默认 0.0.0.0:5000。
//	5001 是**容器**里对外发布的那一层——docker-compose 把 host 5001
//	映射到 container 5000（distribution 在另一个容器，占 registry:5000）。
//	本地没有这层映射，指到 5001 → ECONNREFUSED → Vite 直接回 500。
const apiTarget = process.env.CR_API_PROXY_TARGET ?? 'http://127.0.0.1:5000'

export default defineConfig({
  base: '/',
  plugins: [vue()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: apiTarget,
        changeOrigin: true,
      },
      '/setup': {
        target: apiTarget,
        changeOrigin: true,
        // 浏览器直接访问 /setup 是前端路由，交给 Vite 的 SPA fallback；
        // 只有 /setup/status 与 POST /setup 需要转发给后端。
        bypass(req) {
          const url = req.url ?? ''
          if (req.method === 'GET' && !url.startsWith('/setup/status')) {
            return '/index.html'
          }
          return undefined
        },
      },
    },
  },
  build: {
    outDir: 'dist',
    sourcemap: false,
    chunkSizeWarningLimit: 1500,
  },
})
