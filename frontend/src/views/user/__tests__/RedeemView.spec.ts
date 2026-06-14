import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import RedeemView from '@/views/user/RedeemView.vue'

const authState = vi.hoisted(() => ({
  user: {
    username: 'demo-user',
    balance: undefined,
    concurrency: 2,
  },
  refreshUser: vi.fn(),
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => authState,
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
    showWarning: vi.fn(),
  }),
}))

vi.mock('@/stores/subscriptions', () => ({
  useSubscriptionStore: () => ({
    fetchActiveSubscriptions: vi.fn(),
  }),
}))

vi.mock('@/api', () => ({
  redeemAPI: {
    getHistory: vi.fn().mockResolvedValue([]),
    redeem: vi.fn(),
  },
  authAPI: {
    getPublicSettings: vi.fn().mockResolvedValue({ contact_info: '' }),
  },
}))

vi.mock('@/utils/format', () => ({
  formatDateTime: () => '2026-06-15 00:00',
}))

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key,
    }),
  }
})

describe('RedeemView', () => {
  beforeEach(() => {
    authState.user.balance = undefined
  })

  it('shows placeholder instead of 0.00 before balance is refreshed', async () => {
    const wrapper = mount(RedeemView, {
      global: {
        stubs: {
          AppLayout: { template: '<div><slot /></div>' },
          Icon: true,
          Transition: false,
        },
      },
    })

    await flushPromises()

    expect(wrapper.text()).toContain('...')
    expect(wrapper.text()).not.toContain('$0.00')
  })
})
