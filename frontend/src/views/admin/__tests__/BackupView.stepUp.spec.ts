import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import BackupView from '../BackupView.vue'

const mocks = vi.hoisted(() => ({
  getS3Config: vi.fn(),
  updateS3Config: vi.fn(),
  getSchedule: vi.fn(),
  listBackups: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn(),
}))

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@/api', () => ({
  adminAPI: {
    backup: {
      getS3Config: mocks.getS3Config,
      updateS3Config: mocks.updateS3Config,
      getSchedule: mocks.getSchedule,
      listBackups: mocks.listBackups,
      testS3Connection: vi.fn(),
      getBackup: vi.fn(),
      createBackup: vi.fn(),
      getDownloadURL: vi.fn(),
      restoreBackup: vi.fn(),
      deleteBackup: vi.fn(),
      updateSchedule: vi.fn(),
    },
  },
}))
vi.mock('@/stores', () => ({ useAppStore: () => ({ showError: mocks.showError, showSuccess: mocks.showSuccess, showWarning: vi.fn() }) }))

beforeEach(() => {
  vi.clearAllMocks()
  mocks.getS3Config.mockResolvedValue({ endpoint: '', region: 'auto', bucket: '', access_key_id: '', prefix: 'backups/', force_path_style: false })
  mocks.getSchedule.mockResolvedValue({ enabled: false, cron_expr: '0 2 * * *', retain_days: 14, retain_count: 10 })
  mocks.listBackups.mockResolvedValue({ items: [] })
})

describe('BackupView S3 TOTP step-up', () => {
  it('cancels without submitting S3 configuration', async () => {
    const wrapper = mount(BackupView, { attachTo: document.body })
    await flushPromises()

    await wrapper.get('[data-test="backup-s3-save"]').trigger('click')
    await flushPromises()

    expect(document.querySelector('[data-test="backup-s3-totp-dialog"]')).not.toBeNull()
    ;(document.querySelector('[data-test="backup-s3-totp-cancel"]') as HTMLButtonElement).click()
    await flushPromises()
    expect(mocks.updateS3Config).not.toHaveBeenCalled()
  })

  it('submits once with the TOTP code and surfaces update errors', async () => {
    mocks.updateS3Config.mockRejectedValueOnce({ message: 'invalid TOTP code' })
    const wrapper = mount(BackupView, { attachTo: document.body })
    await flushPromises()

    await wrapper.get('[data-test="backup-s3-save"]').trigger('click')
    const codeInput = document.querySelector('[data-test="backup-s3-totp-code"]') as HTMLInputElement
    codeInput.value = '123456'
    codeInput.dispatchEvent(new Event('input', { bubbles: true }))
    await flushPromises()
    ;(document.querySelector('[data-test="backup-s3-totp-submit"]') as HTMLButtonElement).click()
    await flushPromises()

    expect(mocks.updateS3Config).toHaveBeenCalledTimes(1)
    expect(mocks.updateS3Config.mock.calls[0]?.[0]).toMatchObject({ totp_code: '123456' })
    expect(mocks.showError).toHaveBeenCalledWith('invalid TOTP code')
  })

  it('prevents duplicate S3 updates while submitting', async () => {
    let resolveUpdate: () => void
    mocks.updateS3Config.mockImplementationOnce(() => new Promise<void>((resolve) => { resolveUpdate = resolve }))
    const wrapper = mount(BackupView, { attachTo: document.body })
    await flushPromises()

    await wrapper.get('[data-test="backup-s3-save"]').trigger('click')
    const codeInput = document.querySelector('[data-test="backup-s3-totp-code"]') as HTMLInputElement
    codeInput.value = '123456'
    codeInput.dispatchEvent(new Event('input', { bubbles: true }))
    await flushPromises()
    const submit = document.querySelector('[data-test="backup-s3-totp-submit"]') as HTMLButtonElement
    submit.click()
    submit.click()
    expect(mocks.updateS3Config).toHaveBeenCalledTimes(1)

    resolveUpdate!()
    await flushPromises()
    expect(mocks.showSuccess).toHaveBeenCalledWith('admin.backup.s3.saved')
  })
})
