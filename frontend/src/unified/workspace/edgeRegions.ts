import { messages } from '../../i18n'

/** localStorage key read by `authorizeEdge` when a new session asks for a node. */
export const EDGE_REGION_STORAGE_KEY = 'dreamtrans.edge.region'

/** Region codes the console offers when an administrator adds a node. */
export const EDGE_REGION_CODES = ['ap-southeast-1', 'ap-southeast-2', 'ap-northeast-1', 'eu-west-2', 'us-west-2'] as const

/** A readable place name for a node region; unknown custom codes show as-is. */
export function edgeRegionLabel(region: string): string {
  const names = messages().edgeRegions as Record<string, string | undefined>
  return names[region] ?? region
}

export function readEdgeRegion(): string {
  try {
    return localStorage.getItem(EDGE_REGION_STORAGE_KEY) || 'auto'
  } catch {
    return 'auto'
  }
}

export function writeEdgeRegion(region: string): void {
  try {
    localStorage.setItem(EDGE_REGION_STORAGE_KEY, region)
  } catch {
    // The preference only lasts for this page without storage.
  }
}
