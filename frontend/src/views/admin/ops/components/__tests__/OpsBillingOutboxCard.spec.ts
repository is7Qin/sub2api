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
  terminal: 5,
  oldest_lag: 0,
  max_attempts: 10,
  circuit_open: false,
  permanent_failures: 0,
  backlogged_rounds: 7,
  round_timeouts: 1,
}

const mountCard = (health: OpsBillingOutboxHealth | null, loading = false) =>
  mount(OpsBillingOutboxCard, { props: { health, loading } })

describe('OpsBillingOutboxCard', () => {
  it('renders running status and backlog numbers', () => {
    const wrapper = mountCard(baseHealth)
    expect(wrapper.text()).toContain('admin.ops.billingOutbox.running')
    expect(wrapper.text()).toContain('153') // pending 150 + processing 3
    expect(wrapper.text()).toContain('5')   // terminal 计数
  })

  it('shows circuit-open state with error text', () => {
    const wrapper = mountCard({ ...baseHealth, circuit_open: true, circuit_error: '42P01 table missing' })
    expect(wrapper.text()).toContain('admin.ops.billingOutbox.circuitOpen')
    expect(wrapper.text()).toContain('42P01 table missing')
  })

  it('colors lag red above 1h and yellow above 10min', () => {
    const red = mountCard({ ...baseHealth, oldest_lag: 2 * 3600 * 1e9 })
    expect(red.find('.lag-value').classes()).toContain('text-red-600')
    const yellow = mountCard({ ...baseHealth, oldest_lag: 20 * 60 * 1e9 })
    expect(yellow.find('.lag-value').classes()).toContain('text-yellow-600')
  })

  it('highlights terminal when terminal_alert set', () => {
    const wrapper = mountCard({ ...baseHealth, terminal_alert: 'terminal growth above threshold' })
    expect(wrapper.find('.terminal-alert').classes()).toContain('text-orange-600')
    expect(wrapper.text()).toContain('terminal growth above threshold')
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
