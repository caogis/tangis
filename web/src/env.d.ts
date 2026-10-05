/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** 后端 API 基地址，默认 http://localhost:8080 */
  readonly VITE_API_BASE?: string
  /** 可选 API Key，设置后所有请求携带 X-API-Key 请求头 */
  readonly VITE_API_KEY?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
