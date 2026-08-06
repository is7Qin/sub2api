import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import OpsBillingOutboxCard from '../OpsBillingOutboxCard.vue'
import type { OpsBillingOutboxHealth } from '@/api/admin/ops'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

const baseHealth: OpsBillingOutboxHealth = {
  running: true,
  processed: 123,
  failures: 2,
  pending: 150,
  processing: 3,
  terminal: 7, // 非碰撞取值：与可见文本 153/0m/2/1 无子串重叠，terminal 缺失/归零时可区分
  oldest_lag: 0,
  max_attempts: 10,
  circuit_open: false,
  permanent_failures: 0,
  backlogged_rounds: 7,
  round_timeouts: 1,
}

const mountCard = (health: OpsBillingOutboxHealth | null, loading = false) =>
  mount(OpsBillingOutboxCard, { props: { health, loading } })

// 第三个 grid 单元 = terminal 计数列，其内的 .text-lg 即计数 div（标签为 text-[10px]）
const terminalCount = (wrapper: ReturnType<typeof mountCard>) =>
  wrapper.findAll('.grid-cols-3 > div')[2].find('.text-lg')

describe('OpsBillingOutboxCard', () => {
  it('renders running status and backlog numbers', () => {
    const wrapper = mountCard(baseHealth)
    expect(wrapper.text()).toContain('admin.ops.billingOutbox.running')
    expect(wrapper.text()).toContain('153') // pending 150 + processing 3
    // terminal 计数断言收敛到计数单元（fixture=7）：terminal 缺失/归零/单元被移除时必然失败
    expect(terminalCount(wrapper).text()).toBe('7')
  })

  it('shows circuit-open state with error text', () => {
    const wrapper = mountCard({ ...baseHealth, circuit_open: true, circuit_error: '42P01 table missing' })
    expect(wrapper.text()).toContain('admin.ops.billingOutbox.circuitOpen')
    expect(wrapper.text()).toContain('42P01 table missing')
  })

  it('shows lag uncolored at zero lag', () => {
    const lag = mountCard(baseHealth).find('.lag-value') // baseHealth.oldest_lag: 0
    expect(lag.text()).toBe('0m')
    expect(lag.classes()).not.toContain('text-red-600')
    expect(lag.classes()).not.toContain('text-yellow-600')
  })

  it('colors lag red above 1h and yellow above 10min with strict bounds', () => {
    const red = mountCard({ ...baseHealth, oldest_lag: 2 * 3600 * 1e9 })
    expect(red.find('.lag-value').text()).toBe('2h 0m')
    expect(red.find('.lag-value').classes()).toContain('text-red-600')

    const yellow = mountCard({ ...baseHealth, oldest_lag: 20 * 60 * 1e9 })
    expect(yellow.find('.lag-value').text()).toBe('20m')
    expect(yellow.find('.lag-value').classes()).toContain('text-yellow-600')

    // 严格 > 边界：恰好 1h → 黄不红；恰好 10min → 无色；各 +ε → 升一档
    const hour = mountCard({ ...baseHealth, oldest_lag: 3600 * 1e9 })
    expect(hour.find('.lag-value').text()).toBe('1h 0m')
    expect(hour.find('.lag-value').classes()).toContain('text-yellow-600')
    expect(hour.find('.lag-value').classes()).not.toContain('text-red-600')

    const tenMin = mountCard({ ...baseHealth, oldest_lag: 600 * 1e9 })
    expect(tenMin.find('.lag-value').text()).toBe('10m')
    expect(tenMin.find('.lag-value').classes()).not.toContain('text-yellow-600')
    expect(tenMin.find('.lag-value').classes()).not.toContain('text-red-600')

    expect(mountCard({ ...baseHealth, oldest_lag: 600 * 1e9 + 1 }).find('.lag-value').classes()).toContain('text-yellow-600')
    expect(mountCard({ ...baseHealth, oldest_lag: 3600 * 1e9 + 1 }).find('.lag-value').classes()).toContain('text-red-600')
  })

  it('highlights terminal count when terminal_alert set', () => {
    const wrapper = mountCard({ ...baseHealth, terminal_alert: 'terminal growth above threshold' })
    // 设计核心：terminal 计数列在 terminal_alert 时橙红高亮（横幅仅展示告警文本）
    expect(terminalCount(wrapper).classes()).toContain('text-orange-600')
    expect(terminalCount(wrapper).text()).toBe('7')
    expect(wrapper.text()).toContain('terminal growth above threshold')
  })

  it('keeps terminal count plain when no terminal_alert', () => {
    const wrapper = mountCard(baseHealth)
    expect(terminalCount(wrapper).classes()).not.toContain('text-orange-600')
    expect(terminalCount(wrapper).classes()).toContain('text-gray-900')
  })

  it('toggles details on click and marks instance view', async () => {
    const wrapper = mountCard(baseHealth)
    expect(wrapper.text()).not.toContain('admin.ops.billingOutbox.details.processed')
    await wrapper.find('.billing-details-toggle').trigger('click')
    expect(wrapper.text()).toContain('admin.ops.billingOutbox.details.processed')
    expect(wrapper.text()).toContain('123')
    expect(wrapper.text()).toContain('admin.ops.billingOutbox.instanceView')
  })

  it('shows placeholder while loading', () => {
    const wrapper = mountCard(null, true)
    expect(wrapper.find('.billing-card-loading').exists()).toBe(true)
  })

  it('shows error state when health is null and not loading', () => {
    const wrapper = mountCard(null)
    expect(wrapper.text()).toContain('admin.ops.billingOutbox.error')
  })
})
