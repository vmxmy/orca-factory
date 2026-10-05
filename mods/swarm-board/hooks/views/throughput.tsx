import type { ComponentType, ReactElement } from 'react'
import type { ThroughputData } from '../../types'

declare function h(
  component: ComponentType<{ dimColor?: boolean; children?: string }>,
  props: { dimColor?: boolean },
  children: string,
): ReactElement

export const buildThroughput = (): ThroughputData => ({ hourly: [] })

export const renderThroughput = (Text: ComponentType<{ dimColor?: boolean; children?: string }>) =>
  h(Text, { dimColor: true }, '数据未就绪')
