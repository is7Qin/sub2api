import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

const apiMocks = vi.hoisted(() => ({
  list: vi.fn(), create: vi.fn(), update: vi.fn(),
  getAvailable: vi.fn(), getUserGroupRates: vi.fn(),
  getPublicSettings: vi.fn(), getDashboardApiKeysUsage: vi.fn(),
}))
vi.mock('@/api', () => ({
  keysAPI: { list: apiMocks.list, create: apiMocks.create, update: apiMocks.update },
  userGroupsAPI: { getAvailable: apiMocks.getAvailable, getUserGroupRates: apiMocks.getUserGroupRates },
  authAPI: { getPublicSettings: apiMocks.getPublicSettings },
  usageAPI: { getDashboardApiKeysUsage: apiMocks.getDashboardApiKeysUsage },
}))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn() }) }))
vi.mock('@/stores/onboarding', () => ({ useOnboardingStore: () => ({ isCurrentStep: vi.fn(() => false), nextStep: vi.fn() }) }))
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<div><slot /></div>' } }))
vi.mock('@/components/layout/TablePageLayout.vue', () => ({ default: { template: '<div><slot name="actions"/><slot name="table"/></div>' } }))
vi.mock('@/components/common/DataTable.vue', () => ({
  default: { props: ['data', 'columns'], template: '<div><span v-for="column in columns" :data-testid="`table-header-${column.key}`">{{ column.label }}</span><template v-for="row in data"><slot name="cell-concurrency" :row="row"/><slot name="cell-actions" :row="row"/></template><slot name="empty"/></div>' },
}))
vi.mock('@/components/common/BaseDialog.vue', () => ({ default: { props: ['show'], template: '<div v-if="show"><slot/><slot name="footer"/></div>' } }))
vi.mock('@/components/common/Select.vue', () => ({ default: { template: '<div />' } }))
vi.mock('@/components/common/SearchInput.vue', () => ({ default: { template: '<div />' } }))
vi.mock('@/components/common/EmptyState.vue', () => ({ default: { template: '<div />' } }))

import KeysView from '../KeysView.vue'

const key = {
  id: 5, user_id: 1, key: 'sk-key', name: 'existing', group_id: 1, status: 'active',
  ip_whitelist: [], ip_blacklist: [], concurrency: 7, quota: 0, quota_used: 0,
  expires_at: null, created_at: '', updated_at: '', rate_limit_5h: 0, rate_limit_1d: 0,
  rate_limit_7d: 0, usage_5h: 0, usage_1d: 0, usage_7d: 0, window_5h_start: null,
  window_1d_start: null, window_7d_start: null, reset_5h_at: null, reset_1d_at: null,
  reset_7d_at: null, openai_force_priority_tier: false,
}

async function mountView(items: any[] = []) {
  apiMocks.list.mockResolvedValue({ items, total: items.length, pages: 1 })
  const wrapper = mount(KeysView, { global: { plugins: [createPinia()] } })
  await flushPromises()
  return wrapper
}

beforeEach(() => {
  setActivePinia(createPinia())
  vi.clearAllMocks()
  apiMocks.getAvailable.mockResolvedValue([{ id: 1, name: 'g', description: null, rate_multiplier: 1, subscription_type: 'standard', platform: 'openai' }])
  apiMocks.getUserGroupRates.mockResolvedValue({})
  apiMocks.getPublicSettings.mockResolvedValue({})
  apiMocks.getDashboardApiKeysUsage.mockResolvedValue({ stats: {} })
  apiMocks.create.mockResolvedValue({})
  apiMocks.update.mockResolvedValue({})
})

describe('KeysView concurrency form', () => {
  it('shows the concurrency column and only enabled key usage in a mixed list', async () => {
    const wrapper = await mountView([
      { ...key, id: 5, concurrency: 5, current_concurrency: 2 },
      { ...key, id: 6, concurrency: 0, current_concurrency: 2 },
    ])
    expect(wrapper.get('[data-testid="table-header-concurrency"]').text()).toContain('keys.concurrencyUsage')
    expect(wrapper.get('[data-testid="key-concurrency-usage-5"]').text()).toContain('2 / 5')
    expect(wrapper.find('[data-testid="key-concurrency-usage-6"]').exists()).toBe(false)
  })

  it('hides the concurrency column and usage for an all-zero list', async () => {
    const wrapper = await mountView([{ ...key, concurrency: 0, current_concurrency: 2 }])
    expect(wrapper.find('[data-testid="table-header-concurrency"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="key-concurrency-usage-5"]').exists()).toBe(false)
  })

  it('does not report false zero when live usage is unavailable', async () => {
    const wrapper = await mountView([{ ...key, concurrency: 5, current_concurrency: undefined }])
    expect(wrapper.find('[data-testid="key-concurrency-usage-5"]').exists()).toBe(false)
  })

  it('defaults create concurrency to zero and explains shared user limit', async () => {
    const wrapper = await mountView()
    await wrapper.find('[data-tour="keys-create-btn"]').trigger('click')
    const input = wrapper.get('[data-testid="key-concurrency"]')
    expect((input.element as HTMLInputElement).value).toBe('0')
    expect(input.attributes('min')).toBe('0')
    expect(input.attributes('max')).toBe('2147483647')
    expect(wrapper.text()).toContain('keys.concurrencyHint')
  })

  it('hydrates edit concurrency and saves a positive value', async () => {
    const wrapper = await mountView([key])
    const edit = wrapper.findAll('button').find((button) => button.text() === 'common.edit')!
    await edit.trigger('click')
    const input = wrapper.get('[data-testid="key-concurrency"]')
    expect((input.element as HTMLInputElement).value).toBe('7')
    await input.setValue('9')
    await wrapper.get('#key-form').trigger('submit')
    await flushPromises()
    expect(apiMocks.update).toHaveBeenCalledWith(5, expect.objectContaining({ concurrency: 9 }))
  })

  it('saves explicit zero when editing', async () => {
    const wrapper = await mountView([key])
    await wrapper.findAll('button').find((button) => button.text() === 'common.edit')!.trigger('click')
    await wrapper.get('[data-testid="key-concurrency"]').setValue('0')
    await wrapper.get('#key-form').trigger('submit')
    await flushPromises()
    expect(apiMocks.update).toHaveBeenCalledWith(5, expect.objectContaining({ concurrency: 0 }))
  })

  it.each(['-1', '1.5', '2147483648'])('rejects invalid bounded integer %s', async (value) => {
    const wrapper = await mountView([key])
    await wrapper.findAll('button').find((button) => button.text() === 'common.edit')!.trigger('click')
    await wrapper.get('[data-testid="key-concurrency"]').setValue(value)
    await wrapper.get('#key-form').trigger('submit')
    expect(apiMocks.update).not.toHaveBeenCalled()
  })
})
