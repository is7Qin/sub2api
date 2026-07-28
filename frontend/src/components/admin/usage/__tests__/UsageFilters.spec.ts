import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { enableAutoUnmount, mount, flushPromises } from '@vue/test-utils'

import UsageFilters from '../UsageFilters.vue'

// --- i18n messages (only what UsageFilters needs) ---
const messages: Record<string, string> = {
  'admin.usage.userDeletedBadge': 'deleted',
  'admin.usage.userFilter': 'User',
  'admin.usage.searchUserPlaceholder': 'Search user...',
  'usage.apiKeyFilter': 'API Key',
  'admin.usage.searchApiKeyPlaceholder': 'Search API key...',
  'usage.model': 'Model',
  'admin.usage.allModels': 'All Models',
  'admin.usage.account': 'Account',
  'admin.usage.searchAccountPlaceholder': 'Search account...',
  'usage.type': 'Type',
  'admin.usage.allTypes': 'All Types',
  'usage.ws': 'WS',
  'usage.stream': 'Stream',
  'usage.sync': 'Sync',
  'admin.usage.billingType': 'Billing Type',
  'admin.usage.allBillingTypes': 'All Billing Types',
  'admin.usage.billingTypeBalance': 'Balance',
  'admin.usage.billingTypeSubscription': 'Subscription',
  'admin.usage.billingMode': 'Billing Mode',
  'admin.usage.allBillingModes': 'All Billing Modes',
  'admin.usage.billingModeToken': 'Token',
  'admin.usage.billingModePerRequest': 'Per Request',
  'admin.usage.billingModeImage': 'Image',
  'admin.usage.group': 'Group',
  'admin.usage.allGroups': 'All Groups',
  'common.refresh': 'Refresh',
  'common.reset': 'Reset',
  'admin.usage.cleanup.button': 'Cleanup',
  'usage.exportExcel': 'Export',
}

// Mock vue-i18n
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => messages[key] ?? key,
    }),
  }
})

// Mock the admin API module — we control searchUsers return value per test
const mockSearchUsers = vi.fn()
const mockSearchApiKeys = vi.fn().mockResolvedValue([])
const mockGroupsList = vi.fn().mockResolvedValue({ items: [] })
const mockGetModelStats = vi.fn().mockResolvedValue({ models: [] })
const mockAccountsList = vi.fn().mockResolvedValue({ items: [] })

vi.mock('@/api/admin', () => ({
  adminAPI: {
    usage: {
      searchUsers: (...args: any[]) => mockSearchUsers(...args),
      searchApiKeys: (...args: any[]) => mockSearchApiKeys(...args),
    },
    groups: { list: (...args: any[]) => mockGroupsList(...args) },
    dashboard: { getModelStats: (...args: any[]) => mockGetModelStats(...args) },
    accounts: { list: (...args: any[]) => mockAccountsList(...args) },
  },
}))

// Default props helper
const defaultFilters = () => ({
  user_id: undefined,
  api_key_id: undefined,
  account_id: undefined,
  model: null,
  request_type: null,
  billing_type: null,
  billing_mode: null,
  group_id: null,
  start_date: '',
  end_date: '',
})

function mountFilters(filters = defaultFilters()) {
  return mount(UsageFilters, {
    props: {
      modelValue: filters,
      exporting: false,
      startDate: '2026-05-01',
      endDate: '2026-05-28',
      showActions: false,
      modelOptions: [],
    },
    global: {
      stubs: {
        Select: true,
        Teleport: true,
      },
    },
  })
}

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((promiseResolve, promiseReject) => {
    resolve = promiseResolve
    reject = promiseReject
  })
  return { promise, resolve, reject }
}

enableAutoUnmount(afterEach)

const user = (id: number, email: string) => ({ id, email, deleted: false })

async function startUserSearch(wrapper: ReturnType<typeof mountFilters>, query: string) {
  const input = wrapper.find('input[type="text"]')
  await input.trigger('focus')
  await input.setValue(query)
  vi.advanceTimersByTime(300)
  await flushPromises()
}

describe('UsageFilters — user search dropdown', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    mockSearchUsers.mockReset()
    mockSearchApiKeys.mockResolvedValue([])
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('(a) labels deleted users with the i18n badge and (b) sorts active users before deleted ones, (c) selection sets user_id', async () => {
    // Arrange: mock returns deleted FIRST (proves sorting re-orders to active-first)
    mockSearchUsers.mockResolvedValue([
      { id: 2, email: 'gone@test.com', deleted: true },
      { id: 1, email: 'active@test.com', deleted: false },
    ])

    const wrapper = mountFilters()

    // Trigger focus (sets showUserDropdown = true) then input (fires debounceUserSearch)
    const input = wrapper.find('input[type="text"]')
    await input.trigger('focus')
    await input.setValue('test')
    await input.trigger('input')

    // Advance debounce timer (300ms) then flush the resolved promise
    vi.advanceTimersByTime(300)
    await flushPromises()

    // --- (b) Sort: active user should appear BEFORE deleted user ---
    // Check the underlying component state via rendered DOM order
    const buttons = wrapper.findAll('.usage-filter-dropdown button[type="button"]')
    const emailTexts = buttons.map((b) => b.text())

    // active@test.com should be listed first
    const activeIdx = emailTexts.findIndex((t) => t.includes('active@test.com'))
    const deletedIdx = emailTexts.findIndex((t) => t.includes('gone@test.com'))
    expect(activeIdx).toBeGreaterThanOrEqual(0)
    expect(deletedIdx).toBeGreaterThanOrEqual(0)
    expect(activeIdx).toBeLessThan(deletedIdx)

    // --- (a) Label: deleted user's button shows the badge text ---
    const deletedButton = buttons[deletedIdx]
    expect(deletedButton.text()).toContain('deleted')

    // active user's button does NOT show the badge text
    const activeButton = buttons[activeIdx]
    expect(activeButton.text()).not.toContain('deleted')

    // --- (c) Selection: clicking active user button sets filters.user_id ---
    await activeButton.trigger('click')
    await flushPromises()

    // The component emits 'update:modelValue' or modifies filters.user_id via toRef
    // selectUser sets filters.value.user_id = u.id and emits 'change'
    const changeEmits = wrapper.emitted('change')
    expect(changeEmits).toBeTruthy()
    expect(changeEmits!.length).toBeGreaterThan(0)

    // Also confirm user_id was set by checking the emitted change came through
    // (the component uses toRef so modelValue is mutated in place and 'change' is emitted)
    expect(wrapper.props('modelValue').user_id).toBe(1)
  })

  it('keeps only the latest trimmed query result when responses resolve out of order', async () => {
    const first = deferred<ReturnType<typeof user>[]>()
    const second = deferred<ReturnType<typeof user>[]>()
    mockSearchUsers.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise)
    const wrapper = mountFilters()

    await startUserSearch(wrapper, '  first  ')
    await startUserSearch(wrapper, 'second')
    expect(mockSearchUsers).toHaveBeenNthCalledWith(1, 'first')
    expect(mockSearchUsers).toHaveBeenNthCalledWith(2, 'second')

    second.resolve([user(2, 'second@test.com')])
    await flushPromises()
    first.resolve([user(1, 'first@test.com')])
    await flushPromises()

    expect(wrapper.text()).toContain('second@test.com')
    expect(wrapper.text()).not.toContain('first@test.com')
  })

  it('ignores a stale rejection after newer results are displayed', async () => {
    const first = deferred<ReturnType<typeof user>[]>()
    mockSearchUsers.mockReturnValueOnce(first.promise).mockResolvedValueOnce([user(2, 'new@test.com')])
    const wrapper = mountFilters()

    await startUserSearch(wrapper, 'old')
    await startUserSearch(wrapper, 'new')
    expect(wrapper.text()).toContain('new@test.com')

    first.reject(new Error('stale failure'))
    await flushPromises()
    expect(wrapper.text()).toContain('new@test.com')
  })

  it.each(['clear', 'select', 'external reset', 'setUserKeyword', 'unmount'])('invalidates pending results on %s', async (action) => {
    const pending = deferred<ReturnType<typeof user>[]>()
    mockSearchUsers.mockReturnValueOnce(pending.promise)
    const filters = defaultFilters()
    const wrapper = mountFilters(filters)
    await startUserSearch(wrapper, 'stale')

    if (action === 'clear') {
      await wrapper.find('input[type="text"]').setValue('')
    } else if (action === 'select') {
      await (wrapper.vm as any).selectUser(user(9, 'selected@test.com'))
    } else if (action === 'external reset') {
      filters.user_id = 7
      await wrapper.setProps({ modelValue: { ...filters } })
      filters.user_id = undefined
      await wrapper.setProps({ modelValue: { ...filters } })
    } else if (action === 'setUserKeyword') {
      const vm = wrapper.vm as any
      vm.setUserKeyword('routed@test.com')
    } else {
      wrapper.unmount()
    }

    pending.resolve([user(1, 'stale@test.com')])
    await flushPromises()
    if (action !== 'unmount') expect(wrapper.text()).not.toContain('stale@test.com')
  })
})

describe('UsageFilters — model options come from prop (no dup request)', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    mockGetModelStats.mockClear()
    mockGroupsList.mockClear()
  })
  afterEach(() => { vi.useRealTimers() })

  it('does not call dashboard.getModelStats on mount and renders model options from prop', async () => {
    const wrapper = mount(UsageFilters, {
      props: {
        modelValue: defaultFilters(),
        exporting: false,
        startDate: '2026-05-01',
        endDate: '2026-05-28',
        showActions: false,
        modelOptions: ['claude-3', 'gpt-4o'],
      },
      global: { stubs: { Select: true, Teleport: true } },
    })
    await flushPromises()

    expect(mockGetModelStats).not.toHaveBeenCalled()

    const opts = (wrapper.vm as any).modelOptions as Array<{ value: string | null; label: string }>
    expect(opts.map((o) => o.value)).toEqual([null, 'claude-3', 'gpt-4o'])
  })
})
