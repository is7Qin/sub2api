import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

const apiMocks = vi.hoisted(() => ({
  getUserApiKeys: vi.fn(),
  getAll: vi.fn(),
  updateApiKey: vi.fn(),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    users: { getUserApiKeys: apiMocks.getUserApiKeys },
    groups: { getAll: apiMocks.getAll },
    apiKeys: { updateApiKey: apiMocks.updateApiKey },
  },
}))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn() }) }))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})
vi.mock('@/components/common/BaseDialog.vue', () => ({
  default: { props: ['show'], template: '<div v-if="show"><slot /></div>' },
}))
vi.mock('@/components/common/GroupBadge.vue', () => ({ default: { template: '<span />' } }))
vi.mock('@/components/common/GroupOptionItem.vue', () => ({ default: { template: '<span />' } }))

import UserApiKeysModal from '../UserApiKeysModal.vue'

const key = {
  id: 3, name: 'key', key: 'sk-123456789012345678901234567890', status: 'active', group_id: null,
  concurrency: 4, created_at: '2026-01-01T00:00:00Z',
}

async function mountOpen() {
  const wrapper = mount(UserApiKeysModal, {
    props: { show: false, user: { id: 2, email: 'u@example.com', username: 'u' } as any },
  })
  await wrapper.setProps({ show: true })
  await flushPromises()
  return wrapper
}

describe('UserApiKeysModal concurrency editor', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    apiMocks.getUserApiKeys.mockResolvedValue({ items: [key] })
    apiMocks.getAll.mockResolvedValue([])
    apiMocks.updateApiKey.mockResolvedValue({ api_key: { ...key, concurrency: 0 }, auto_granted_group_access: false })
  })

  it('hydrates concurrency, explains zero, and saves explicit zero', async () => {
    const wrapper = await mountOpen()
    const input = wrapper.get('[data-testid="admin-key-concurrency-3"]')
    expect((input.element as HTMLInputElement).value).toBe('4')
    expect(input.attributes('min')).toBe('0')
    expect(input.attributes('max')).toBe('2147483647')
    expect(wrapper.text()).toContain('admin.users.apiKeyConcurrencyHint')

    await input.setValue('0')
    await wrapper.get('[data-testid="admin-key-concurrency-save-3"]').trigger('click')
    await flushPromises()
    expect(apiMocks.updateApiKey).toHaveBeenCalledWith(3, { concurrency: 0 })
  })

  it.each(['-1', '1.5', '2147483648'])('rejects invalid bounded integer %s', async (value) => {
    const wrapper = await mountOpen()
    await wrapper.get('[data-testid="admin-key-concurrency-3"]').setValue(value)
    await wrapper.get('[data-testid="admin-key-concurrency-save-3"]').trigger('click')
    expect(apiMocks.updateApiKey).not.toHaveBeenCalled()
  })
})
