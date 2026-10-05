/**
 * 分发资源 URL 解析（纯函数，独立模块便于单测）。
 *
 * 背景：后端 /api/v1/services 返回的 tileset_url / wmts_url 是相对路径
 * （如 /services/t1/tileset.json?sig=...），若前端原样透传给 Cesium，
 * 浏览器会解析到 Vite dev 源（5173）拿到 HTML。需拼接 VITE_API_BASE
 * 指向后端源（8080）。
 */

/**
 * 把后端返回的资源 URL 解析为浏览器可直接请求的绝对 URL。
 *
 * 拼接语义为字符串前接（非 RFC 3986 路径替换）：适用于网关/反向代理
 * 保持前缀一致的部署（base 带路径前缀时，后端相对路径整体挂在前缀后）。
 *
 * 规则：
 * - 空串原样返回；
 * - 绝对 URL（http:// 等 scheme）与协议相对 //host 原样返回；
 * - 以 / 开头的源内绝对路径拼 base（base 末尾多余 / 去除，避免出现双斜杠；
 *   base 为空或 url 已含 base 前缀时不重复拼接，防止双重拼接）；
 * - 其余相对路径（如 data/inner.json）交由浏览器按当前页面解析，原样返回。
 */
export function resolveAssetUrl(url: string, base: string): string {
  if (!url) return url
  if (/^[a-zA-Z][a-zA-Z0-9+.-]*:/.test(url) || url.startsWith('//')) return url
  if (url.startsWith('/')) {
    const trimmedBase = (base || '').replace(/\/+$/, '')
    if (!trimmedBase || url === trimmedBase || url.startsWith(trimmedBase + '/')) return url
    return trimmedBase + url
  }
  return url
}

/**
 * 从分发 URL 的 query 中解析防盗链签名过期时间（expires 参数，unix 秒）。
 * 无 expires 或值非法时返回 null（调用方如实展示"未知"，不估算）。
 */
export function parseSigExpiry(url: string): number | null {
  if (!url) return null
  const q = url.indexOf('?')
  if (q < 0) return null
  const raw = new URLSearchParams(url.slice(q + 1)).get('expires')
  if (raw == null || raw === '') return null
  const n = Number(raw)
  return Number.isFinite(n) && n > 0 ? n : null
}
