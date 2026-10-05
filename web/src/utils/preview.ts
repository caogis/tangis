/**
 * 预览地址构造：列表里点「预览」时新页面打开**该任务**的三维预览。
 *
 * 使用 /preview/full（无应用外壳，独占整屏）并带上 ?task=<taskId>，
 * 预览页就绪后会自动加载该任务的服务，用户无需再点一次。
 */
export function taskPreviewUrl(taskId: string): string {
  return `/preview/full?task=${encodeURIComponent(taskId)}`
}
