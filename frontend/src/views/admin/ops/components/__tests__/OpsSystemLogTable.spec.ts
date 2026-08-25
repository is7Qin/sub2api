import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import enLocale from '@/i18n/locales/en'
import zhLocale from '@/i18n/locales/zh'
import OpsSystemLogTable from '../OpsSystemLogTable.vue'

const { cleanupSystemLogs, listSystemLogs, getSystemLogSinkHealth, getRuntimeLogConfig, showError, showSuccess } = vi.hoisted(() => ({
  cleanupSystemLogs: vi.fn(),
  listSystemLogs: vi.fn(),
  getSystemLogSinkHealth: vi.fn(),
  getRuntimeLogConfig: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn(),
}))

vi.mock('@/api/admin/ops', () => ({
  opsAPI: {
    cleanupSystemLogs,
    listSystemLogs,
    getSystemLogSinkHealth,
    getRuntimeLogConfig,
  },
}))

vi.mock('@/stores', () => ({ useAppStore: () => ({ showError, showSuccess }) }))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

const mountTable = () => mount(OpsSystemLogTable, {
  global: { stubs: { Pagination: true, Select: true } },
})

const cleanupButton = (wrapper: ReturnType<typeof mountTable>) =>
  wrapper.findAll('button').find((button) => button.text().includes('按当前筛选清理'))!

describe('OpsSystemLogTable cleanup errors', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
    cleanupSystemLogs.mockReset()
    listSystemLogs.mockReset().mockResolvedValue({ items: [], total: 0 })
    getSystemLogSinkHealth.mockReset().mockResolvedValue({
      queue_depth: 0, queue_capacity: 0, dropped_count: 0, write_failed_count: 0,
      written_count: 0, avg_write_delay_ms: 0,
    })
    getRuntimeLogConfig.mockReset().mockResolvedValue({
      level: 'info', enable_sampling: false, sampling_initial: 100,
      sampling_thereafter: 100, caller: true, stacktrace_level: 'error', retention_days: 30,
    })
    showError.mockReset()
    showSuccess.mockReset()
  })

  it('does not call cleanup when confirmation is cancelled', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(false)
    const wrapper = mountTable()
    await flushPromises()
    await cleanupButton(wrapper).trigger('click')
    expect(cleanupSystemLogs).not.toHaveBeenCalled()
  })

  it.each([
    { reason: 'OPS_SYSTEM_LOG_CLEANUP_FILTER_REQUIRED', message: 'backend detail' },
    { response: { data: { code: 'OPS_SYSTEM_LOG_CLEANUP_FILTER_REQUIRED', detail: 'legacy detail' } } },
    { response: { data: { code: 400, reason: 'OPS_SYSTEM_LOG_CLEANUP_FILTER_REQUIRED', detail: 'legacy reason detail' } } },
  ])('maps the allowlisted normalized or legacy code to an actionable localized error', async (error) => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    cleanupSystemLogs.mockRejectedValue(error)
    const wrapper = mountTable()
    await flushPromises()
    await cleanupButton(wrapper).trigger('click')
    await flushPromises()
    expect(showError).toHaveBeenCalledWith('admin.ops.systemLogs.cleanupFilterRequired')
  })

  it.each([
    { reason: 'INTERNAL_SQL_ERROR', message: 'SELECT secret FROM users' },
    { response: { data: { code: 'UNKNOWN', detail: 'filter: password=secret' } } },
    new Error('private backend failure'),
  ])('uses only the generic localized error for non-allowlisted backend details', async (error) => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    cleanupSystemLogs.mockRejectedValue(error)
    const wrapper = mountTable()
    await flushPromises()
    await cleanupButton(wrapper).trigger('click')
    await flushPromises()
    expect(showError).toHaveBeenCalledWith('admin.ops.systemLogs.cleanupFailed')
  })

  it('keeps the success count, resets pagination, and refreshes logs', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    cleanupSystemLogs.mockResolvedValue({ deleted: 7 })
    const wrapper = mountTable()
    await flushPromises()
    ;(wrapper.vm as any).page = 3
    listSystemLogs.mockClear()
    await cleanupButton(wrapper).trigger('click')
    await flushPromises()
    expect(showSuccess).toHaveBeenCalledWith('清理完成，删除 7 条日志')
    expect((wrapper.vm as any).page).toBe(1)
    expect(listSystemLogs).toHaveBeenCalled()
  })
})

describe('OpsSystemLogTable cleanup locale keys', () => {
  it.each([enLocale, zhLocale])('defines generic and actionable cleanup errors', (locale) => {
    expect(locale.admin.ops.systemLogs.cleanupFailed).toBeTruthy()
    expect(locale.admin.ops.systemLogs.cleanupFilterRequired).toBeTruthy()
  })
})
