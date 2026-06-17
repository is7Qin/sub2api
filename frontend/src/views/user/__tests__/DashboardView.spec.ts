import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import DashboardView from '@/views/user/DashboardView.vue'

const refreshUser = vi.hoisted(() => vi.fn().mockResolvedValue(undefined))
const getDashboardStats = vi.hoisted(() => vi.fn())
const getDashboardTrend = vi.hoisted(() => vi.fn())
const getDashboardModels = vi.hoisted(() => vi.fn())
const getByDateRange = vi.hoisted(() => vi.fn())
const getMyPlatformQuotas = vi.hoisted(() => vi.fn())

const authState = vi.hoisted(() => ({
  user: {
    username: 'demo-user',
    balance: undefined as number | undefined,
  },
  isSimpleMode: false,
  refreshUser,
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => authState,
}))

vi.mock('@/api/usage', () => ({
  usageAPI: {
    getDashboardStats,
    getDashboardTrend,
    getDashboardModels,
    getByDateRange,
  },
}))

vi.mock('@/api/user', () => ({
  getMyPlatformQuotas,
}))

const StatsStub = defineComponent({
  props: {
    balance: {
      type: Number,
      required: false,
    },
  },
  template: '<div data-testid="dashboard-balance">{{ balance === undefined ? "undefined" : balance }}</div>',
})

describe('DashboardView', () => {
  beforeEach(() => {
    authState.user.balance = undefined
    refreshUser.mockReset().mockResolvedValue(undefined)
    getDashboardStats.mockReset().mockResolvedValue({
      total_api_keys: 1,
      active_api_keys: 1,
      today_requests: 0,
      total_requests: 0,
      today_actual_cost: 0,
      today_cost: 0,
      total_actual_cost: 0,
      total_cost: 0,
      today_tokens: 0,
      today_input_tokens: 0,
      today_output_tokens: 0,
      total_tokens: 0,
      total_input_tokens: 0,
      total_output_tokens: 0,
      rpm: 0,
      tpm: 0,
      average_duration_ms: 0,
      by_platform: [],
    })
    getDashboardTrend.mockReset().mockResolvedValue({ trend: [] })
    getDashboardModels.mockReset().mockResolvedValue({ models: [] })
    getByDateRange.mockReset().mockResolvedValue({ items: [] })
    getMyPlatformQuotas.mockReset().mockResolvedValue({ platform_quotas: [] })
  })

  it('does not coerce an unknown balance to zero before refresh completes', async () => {
    const wrapper = mount(DashboardView, {
      global: {
        stubs: {
          AppLayout: { template: '<div><slot /></div>' },
          LoadingSpinner: true,
          UserDashboardStats: StatsStub,
          UserDashboardCharts: true,
          UserDashboardRecentUsage: true,
          UserDashboardQuickActions: true,
        },
      },
    })

    await flushPromises()

    expect(wrapper.get('[data-testid="dashboard-balance"]').text()).toBe('undefined')
  })
})
