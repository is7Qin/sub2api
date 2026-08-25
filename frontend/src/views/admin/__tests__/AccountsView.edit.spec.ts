import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import AccountsView from '../AccountsView.vue'

const {
  listAccounts,
  listWithEtag,
  getById,
  getBatchTodayStats,
  getAllProxies,
  getAllGroups,
  showError
} = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  listWithEtag: vi.fn(),
  getById: vi.fn(),
  getBatchTodayStats: vi.fn(),
  getAllProxies: vi.fn(),
  getAllGroups: vi.fn(),
  showError: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      list: listAccounts,
      listWithEtag,
      getById,
      getBatchTodayStats,
      delete: vi.fn(),
      batchClearError: vi.fn(),
      batchRefresh: vi.fn(),
      toggleSchedulable: vi.fn()
    },
    proxies: {
      getAll: getAllProxies
    },
    groups: {
      getAll: getAllGroups
    }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError,
    showSuccess: vi.fn(),
    showInfo: vi.fn()
  })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    token: 'test-token',
    isSimpleMode: false
  })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

const DataTableStub = {
  props: ['data'],
  template: `
    <div data-test="data-table">
      <div v-for="row in data" :key="row.id" data-test="account-row" :data-rpm="row.current_rpm">
        <slot name="cell-actions" :row="row" />
      </div>
    </div>
  `
}

const EditAccountModalStub = {
  name: 'EditAccountModal',
  props: ['show', 'account'],
  emits: ['updated'],
  template: `
    <div
      data-test="edit-account-modal"
      :data-show="String(show)"
      :data-account-name="account?.name ?? ''"
      :data-extra-config="account?.extra?.durable_config ?? ''"
    />
  `
}

const makeAccount = (overrides: Record<string, unknown> = {}) => ({
  id: 1,
  name: 'List projection',
  platform: 'anthropic',
  type: 'oauth',
  proxy_id: null,
  concurrency: 1,
  priority: 0,
  status: 'active',
  error_message: null,
  last_used_at: null,
  expires_at: null,
  auto_pause_on_expired: false,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
  schedulable: true,
  rate_limited_at: null,
  rate_limit_reset_at: null,
  overload_until: null,
  temp_unschedulable_until: null,
  temp_unschedulable_reason: null,
  session_window_start: null,
  session_window_end: null,
  session_window_status: null,
  ...overrides
})

const mountView = () => mount(AccountsView, {
  global: {
    stubs: {
      AppLayout: { template: '<div><slot /></div>' },
      TablePageLayout: {
        template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>'
      },
      DataTable: DataTableStub,
      Pagination: true,
      ConfirmDialog: true,
      AccountTableActions: { template: '<div><slot name="beforeCreate" /><slot name="after" /></div>' },
      AccountTableFilters: true,
      AccountBulkActionsBar: true,
      AccountActionMenu: true,
      ImportDataModal: true,
      ReAuthAccountModal: true,
      AccountTestModal: true,
      AccountStatsModal: true,
      ScheduledTestsPanel: true,
      SyncFromCrsModal: true,
      TempUnschedStatusModal: true,
      ErrorPassthroughRulesModal: true,
      TLSFingerprintProfilesModal: true,
      CreateAccountModal: true,
      EditAccountModal: EditAccountModalStub,
      BulkEditAccountModal: true,
      PlatformTypeBadge: true,
      AccountCapacityCell: true,
      AccountStatusIndicator: true,
      AccountTodayStatsCell: true,
      AccountGroupsCell: true,
      AccountUsageCell: true,
      Icon: true
    }
  }
})

describe('admin AccountsView editing', () => {
  beforeEach(() => {
    localStorage.clear()

    listAccounts.mockReset()
    listWithEtag.mockReset()
    getById.mockReset()
    getBatchTodayStats.mockReset()
    getAllProxies.mockReset()
    getAllGroups.mockReset()
    showError.mockReset()

    listAccounts.mockResolvedValue({
      items: [makeAccount({ extra: { list_only: 'incomplete' }, current_rpm: 17 })],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1
    })
    listWithEtag.mockResolvedValue({
      notModified: true,
      etag: null,
      data: null
    })
    getBatchTodayStats.mockResolvedValue({ stats: {} })
    getAllProxies.mockResolvedValue([])
    getAllGroups.mockResolvedValue([])
  })

  it('loads canonical account details before opening the edit modal', async () => {
    getById.mockResolvedValue(makeAccount({
      name: 'Canonical account',
      extra: { durable_config: 'preserved' }
    }))
    const wrapper = mountView()

    await flushPromises()
    await wrapper.get('[data-test="edit-account"]').trigger('click')
    await flushPromises()

    expect(getById).toHaveBeenCalledWith(1)
    expect(wrapper.get('[data-test="edit-account-modal"]').attributes('data-show')).toBe('true')
    expect(wrapper.get('[data-test="edit-account-modal"]').attributes('data-account-name')).toBe('Canonical account')
    expect(wrapper.get('[data-test="edit-account-modal"]').attributes('data-extra-config')).toBe('preserved')
  })

  it('keeps the edit modal closed and reports a failed detail request', async () => {
    getById.mockRejectedValue(new Error('Unable to load account details'))
    const wrapper = mountView()

    await flushPromises()
    await wrapper.get('[data-test="edit-account"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-test="edit-account-modal"]').attributes('data-show')).toBe('false')
    expect(showError).toHaveBeenCalledWith('Unable to load account details')
  })

  it('preserves current RPM when an updated account response omits it', async () => {
    const wrapper = mountView()

    await flushPromises()
    await wrapper.getComponent(EditAccountModalStub).vm.$emit('updated', makeAccount({
      name: 'Updated account',
      current_rpm: undefined
    }))
    await flushPromises()

    expect(wrapper.get('[data-test="account-row"]').attributes('data-rpm')).toBe('17')
  })
})
