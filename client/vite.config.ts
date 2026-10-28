import react from '@vitejs/plugin-react'
import { defineConfig, loadEnv, type Plugin } from 'vite'

// 只在 production build 注入 CSP：dev 時 plugin-react 會注入 inline script、
// Vite 會注入 <style>，會被 CSP 擋掉。
function cspPlugin(env: Record<string, string>): Plugin {
  const apiUrl = env.VITE_API_URL || 'http://localhost:8080'
  const wsUrl = env.VITE_WS_URL || 'ws://localhost:8080/ws'
  const connectSrc = [
    "'self'",
    new URL(apiUrl).origin,
    new URL(wsUrl).origin,
  ].join(' ')
  const policy = [
    "default-src 'self'",
    "script-src 'self'",
    "style-src 'self'",
    `connect-src ${connectSrc}`,
    "img-src 'self' data:",
    "object-src 'none'",
    "base-uri 'self'",
    "form-action 'self'",
  ].join('; ')
  return {
    name: 'inject-csp',
    apply: 'build',
    transformIndexHtml() {
      return [
        {
          tag: 'meta',
          attrs: { 'http-equiv': 'Content-Security-Policy', content: policy },
          injectTo: 'head-prepend',
        },
      ]
    },
  }
}

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), 'VITE_')
  return {
    base: '/danmaku/',
    plugins: [react(), cspPlugin(env)],
  }
})
