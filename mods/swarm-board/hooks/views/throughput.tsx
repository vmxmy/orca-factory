import type { ComponentType } from 'react'
import type { ThroughputData } from '../../types'

export const buildThroughput = (): ThroughputData => ({ hourly: [] })

export const renderThroughput = (Text: ComponentType<{ dimColor?: boolean; children?: string }>) => (
  <Text dimColor>数据未就绪</Text>
)
