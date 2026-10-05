/**
 * 内联 SVG 图标路径表（stroke 风格，24x24 视图框，随字色继承）。
 * 只收录界面实际用到的图标，避免引入整套图标库带来的体积与风格不一致。
 */
export type IconName =
  | 'dashboard' | 'import' | 'vector' | 'convert' | 'qc' | 'edit'
  | 'services' | 'globe' | 'tasks' | 'settings'
  | 'sun' | 'moon' | 'check' | 'alert' | 'close' | 'refresh'
  | 'copy' | 'external' | 'play' | 'pause' | 'trash' | 'chevron-right' | 'search' | 'layers' | 'edition'
  | 'shield'

export const ICON_PATHS: Record<IconName, string> = {
  dashboard: 'M3 3h7v7H3zM14 3h7v5h-7zM14 12h7v9h-7zM3 14h7v7H3z',
  import: 'M12 3v12M7 10l5 5 5-5M4 21h16',
  vector: 'M3 6l6-3 6 3 6-3v15l-6 3-6-3-6 3zM9 3v15M15 6v15',
  convert: 'M12 3l8 4.5v9L12 21l-8-4.5v-9zM12 12l8-4.5M12 12v9M12 12L4 7.5',
  qc: 'M12 3l7 3v6c0 4-3 7-7 9-4-2-7-5-7-9V6zM9 12l2 2 4-4',
  edit: 'M4 20h4l10-10-4-4L4 16zM14 6l4 4',
  services: 'M4 5h16v5H4zM4 14h16v5H4zM8 7.5h.01M8 16.5h.01',
  globe: 'M12 3a9 9 0 100 18 9 9 0 000-18zM3 12h18M12 3c2.5 3 2.5 15 0 18M12 3c-2.5 3-2.5 15 0 18',
  tasks: 'M4 6h16M4 12h16M4 18h10',
  settings: 'M12 15a3 3 0 100-6 3 3 0 000 6zM19.4 15a1.7 1.7 0 00.3 1.9l.1.1a2 2 0 11-2.8 2.8l-.1-.1a1.7 1.7 0 00-2.9 1.2 2 2 0 11-4 0 1.7 1.7 0 00-2.9-1.2l-.1.1a2 2 0 11-2.8-2.8l.1-.1A1.7 1.7 0 004.6 15a2 2 0 010-4 1.7 1.7 0 001.2-2.9l-.1-.1a2 2 0 112.8-2.8l.1.1A1.7 1.7 0 0011.5 4a2 2 0 014 0 1.7 1.7 0 002.9 1.2l.1-.1a2 2 0 112.8 2.8l-.1.1A1.7 1.7 0 0019.4 11a2 2 0 010 4z',
  sun: 'M12 17a5 5 0 100-10 5 5 0 000 10zM12 1v2M12 21v2M4.2 4.2l1.4 1.4M18.4 18.4l1.4 1.4M1 12h2M21 12h2M4.2 19.8l1.4-1.4M18.4 5.6l1.4-1.4',
  moon: 'M21 12.8A9 9 0 1111.2 3a7 7 0 009.8 9.8z',
  check: 'M4 12l5 5L20 6',
  alert: 'M12 8v5M12 16.5h.01M10.3 3.9L2.6 17a2 2 0 001.7 3h15.4a2 2 0 001.7-3L13.7 3.9a2 2 0 00-3.4 0z',
  close: 'M6 6l12 12M18 6L6 18',
  refresh: 'M21 12a9 9 0 11-3-6.7M21 4v5h-5',
  copy: 'M9 9h10v10H9zM5 15V5h10',
  external: 'M14 4h6v6M20 4l-9 9M18 14v5a1 1 0 01-1 1H5a1 1 0 01-1-1V7a1 1 0 011-1h5',
  play: 'M7 4l12 8-12 8z',
  pause: 'M9 5v14M15 5v14',
  trash: 'M4 7h16M9 7V4h6v3M6 7l1 14h10l1-14',
  'chevron-right': 'M9 5l7 7-7 7',
  search: 'M11 19a8 8 0 100-16 8 8 0 000 16zM21 21l-4.3-4.3',
  layers: 'M12 3l9 5-9 5-9-5zM3 13l9 5 9-5M3 17l9 5 9-5',
  edition: 'M12 15a4 4 0 100-8 4 4 0 000 8zM8.5 14L6 21l6-3 6 3-2.5-7',
  shield: 'M12 3l7 3v6c0 4-3 7-7 9-4-2-7-5-7-9V6z',
}
