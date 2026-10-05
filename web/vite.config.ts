import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import cesium from 'vite-plugin-cesium'

// Cesium 静态资源（Workers/Assets/Widgets/ThirdParty）由 vite-plugin-cesium
// 在 dev 与 build 两个阶段自动拷贝并注入 CESIUM_BASE_URL。
export default defineConfig({
  plugins: [vue(), cesium()],
  server: {
    port: 5173,
  },
})
