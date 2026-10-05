import type { ComponentType } from 'react'
import type { GraphData } from '../../types'

export const buildGraph = (): GraphData => ({ criticalPath: [], unlockRank: [] })

export const renderGraph = (Text: ComponentType<{ dimColor?: boolean; children?: string }>) => (
  <Text dimColor>数据未就绪</Text>
)
