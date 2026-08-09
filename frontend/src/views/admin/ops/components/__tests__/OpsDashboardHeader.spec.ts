import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import OpsDashboardHeader from '../OpsDashboardHeader.vue'
import type { OpsBillingOutboxHealth } from '@/api/admin/ops'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})
vi.mock('@/stores', () => ({
  useAdminSettingsStore: () => ({
    opsRealtimeMonitoringEnabled: false,
    setOpsRealtimeMonitoringEnabledLocal: vi.fn()
  })
}))
// onMounted 会拉取 group 列表；mock 掉避免测试输出网络错误噪音
vi.mock('@/api', () => ({
  adminAPI: { groups: { getAll: vi.fn().mockResolvedValue([]) } }
}))

const baseHealth: OpsBillingOutboxHealth = {
  running: true, processed: 0, failures: 0, pending: 0, processing: 0,
  terminal: 0, oldest_lag: 0, max_attempts: 10, circuit_open: false,
  permanent_failures: 0, backlogged_rounds: 0, round_timeouts: 0,
}

const mountHeader = (billingHealth: OpsBillingOutboxHealth | null | undefined) =>
  mount(OpsDashboardHeader, {
    props: {
      // OpsDashboardOverview 必填字段过多，测试只关心诊断逻辑；
      // qps.current > 0 与 sla 0.99 使 isSystemIdle 为 false（否则诊断提前返回 idle 项）。
      overview: {
        system_metrics: { cpu_usage_percent: 0, memory_usage_percent: 0 },
        qps: { current: 1, peak: 1, avg: 1 },
        sla: 0.99,
      } as any,
      platform: '',
      groupId: null,
      timeRange: '1h',
      queryMode: 'auto',
      loading: false,
      lastUpdated: null,
      billingHealth,
    },
    global: { stubs: { Icon: true } },
  })

describe('OpsDashboardHeader billing diagnosis', () => {
  it('adds critical diagnosis when circuit is open', () => {
    const wrapper = mountHeader({ ...baseHealth, circuit_open: true, circuit_error: '42P01' })
    expect(wrapper.text()).toContain('admin.ops.diagnosis.circuitBreakerOpen')
  })

  it('adds critical diagnosis when terminal alert is set', () => {
    const wrapper = mountHeader({ ...baseHealth, terminal_alert: 'growth' })
    expect(wrapper.text()).toContain('admin.ops.diagnosis.terminalAlert')
  })

  it('adds warning diagnosis when lag exceeds 1h', () => {
    const wrapper = mountHeader({ ...baseHealth, oldest_lag: 2 * 3600 * 1e9 })
    expect(wrapper.text()).toContain('admin.ops.diagnosis.billingLagCritical')
  })

  it('does not add billing diagnosis at exactly 1h lag', () => {
    const wrapper = mountHeader({ ...baseHealth, oldest_lag: 3600 * 1e9 })
    expect(wrapper.text()).not.toContain('admin.ops.diagnosis.billingLagCritical')
  })

  it('adds no billing diagnosis when billingHealth is null or undefined', () => {
    const nullWrapper = mountHeader(null)
    expect(nullWrapper.text()).not.toContain('admin.ops.diagnosis.circuitBreakerOpen')
    const undefinedWrapper = mountHeader(undefined)
    expect(undefinedWrapper.text()).not.toContain('admin.ops.diagnosis.billingLagCritical')
  })
})
