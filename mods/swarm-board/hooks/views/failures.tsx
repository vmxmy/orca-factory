import type { ComponentType } from 'react'
import type { FailureData } from '../../types'

export const buildFailures = (): FailureData => ({ topCorrections: [], repeatedFails: [] })

export const renderFailures = (Text: ComponentType<{ dimColor?: boolean; children?: string }>) => (
  <Text dimColor>数据未就绪</Text>
)
