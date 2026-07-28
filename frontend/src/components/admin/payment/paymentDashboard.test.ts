import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import OrderStatsCards from './OrderStatsCards.vue'
import PaymentMethodChart from './PaymentMethodChart.vue'
import TopUsersLeaderboard from './TopUsersLeaderboard.vue'
import type { DashboardStats } from '@/types/payment'

const stats: DashboardStats = {
  today_amount: { USD: 10, CNY: 10 },
  total_amount: { USD: 10, CNY: 10 },
  avg_amount: { USD: 10, CNY: 10 },
  today_count: 2,
  total_count: 2,
  daily_series: [],
  payment_methods: [],
  top_users: {},
}

describe('payment dashboard currency amounts', () => {
  it('renders each currency independently in stable order and tolerates invalid codes', () => {
    const wrapper = mount(OrderStatsCards, {
      props: { stats: { ...stats, today_amount: { USD: 10, CNY: 10, INVALID_CODE: 4 } } },
      global: {
        stubs: { Icon: true },
        plugins: [createI18n({ legacy: false, locale: 'en', messages: { en: {} } })],
      },
    })
    const text = wrapper.text()
    expect(text).toContain('CN¥10.00')
    expect(text).toContain('US$10.00')
    expect(text).toContain('INVALID_CODE 4.00')
    expect(text.indexOf('CN¥')).toBeLessThan(text.indexOf('US$'))
    expect(text).not.toContain('20.00')
  })

  it('keeps payment-method amounts and scales separate by currency', () => {
    const wrapper = mount(PaymentMethodChart, {
      props: {
        methods: [
          { type: 'stripe', amount: { USD: 10, INVALID_CODE: 4 }, count: 2 },
          { type: 'alipay', amount: { USD: 5, CNY: 10 }, count: 2 },
        ],
      },
      global: {
        plugins: [createI18n({ legacy: false, locale: 'en', messages: { en: {} } })],
      },
    })

    expect(wrapper.text()).toContain('INVALID_CODE 4.00')
    expect(wrapper.text()).not.toContain('USD 15.00')
    const widths = wrapper.findAll('[style]').map(node => node.attributes('style'))
    expect(widths).toContain('width: 100%;')
    expect(widths).toContain('width: 50%;')
  })

  it('renders currency leaderboards independently and tolerates invalid codes', () => {
    const wrapper = mount(TopUsersLeaderboard, {
      props: {
        users: {
          USD: [{ user_id: 1, email: 'usd@example.com', amount: 10 }],
          INVALID_CODE: [{ user_id: 2, email: 'invalid@example.com', amount: 4 }],
        },
      },
      global: {
        plugins: [createI18n({ legacy: false, locale: 'en', messages: { en: {} } })],
      },
    })

    const text = wrapper.text()
    expect(text).toContain('INVALID_CODE 4.00')
    expect(text.indexOf('INVALID_CODE')).toBeLessThan(text.indexOf('USD'))
    expect(text).not.toContain('14.00')
  })
})
