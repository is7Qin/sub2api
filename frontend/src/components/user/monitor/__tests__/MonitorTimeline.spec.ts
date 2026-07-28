import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import MonitorTimeline from '../MonitorTimeline.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

vi.mock('@/composables/useChannelMonitorFormat', () => ({
  useChannelMonitorFormat: () => ({
    statusLabel: (status: string) => status,
    formatLatency: (latency: number) => String(latency),
    formatRelativeTime: (time: string) => time,
  }),
}))

describe('MonitorTimeline narrow layout', () => {
  it('renders one 60-bar tree whose flex items may shrink without changing bar semantics', () => {
    const wrapper = mount(MonitorTimeline, {
      props: {
        countdownSeconds: 5,
        buckets: [{ status: 'operational', latency_ms: 42, checked_at: 'now' }],
      },
    })

    const bars = wrapper.findAll('[title]')
    expect(bars).toHaveLength(60)
    expect(bars.every((bar) => bar.classes().includes('min-w-0'))).toBe(true)
    expect(bars.some((bar) => bar.classes().includes('min-w-[3px]'))).toBe(false)
    expect(bars[59].attributes('style')).toContain('height: 100%')
    expect(bars[59].attributes('title')).toBe('now · operational · 42ms')
  })

  it('preserves the single maintenance state instead of rendering bars', () => {
    const wrapper = mount(MonitorTimeline, { props: { countdownSeconds: 5, maintenance: true } })
    expect(wrapper.findAll('[title]')).toHaveLength(0)
    expect(wrapper.text()).toContain('monitorCommon.maintenancePaused')
  })
})
