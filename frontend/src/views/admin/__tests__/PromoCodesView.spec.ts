import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import PromoCodesView from '../PromoCodesView.vue'

const NativeDate = Date
const timezone = process.env.TEST_TIMEZONE === 'America/New_York' ? 'America/New_York' : 'Asia/Shanghai'

const localParts = (value: Date) => Object.fromEntries(
  new Intl.DateTimeFormat('en-US', {
    timeZone: timezone,
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23',
  }).formatToParts(value).filter((part) => part.type !== 'literal').map((part) => [part.type, Number(part.value)])
)

const parseZonedLocal = (value: string) => {
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})$/.exec(value)
  if (!match) return NativeDate.parse(value)
  const [, year, month, day, hour, minute] = match.map(Number)
  const target = NativeDate.UTC(year, month - 1, day, hour, minute)
  let instant = target
  // Iterate because the New York offset differs across the DST boundary.
  for (let i = 0; i < 2; i += 1) {
    const parts = localParts(new NativeDate(instant))
    const displayed = NativeDate.UTC(parts.year, parts.month - 1, parts.day, parts.hour, parts.minute)
    instant += target - displayed
  }
  const resolved = localParts(new NativeDate(instant))
  return resolved.year === year && resolved.month === month && resolved.day === day
    && resolved.hour === hour && resolved.minute === minute
    ? instant
    : NaN
}

class ZonedDate extends NativeDate {
  constructor(value?: string | number | Date) {
    super(typeof value === 'string' && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/.test(value)
      ? parseZonedLocal(value)
      : value === undefined ? NativeDate.now() : value)
  }
  getFullYear() { return localParts(this).year }
  getMonth() { return localParts(this).month - 1 }
  getDate() { return localParts(this).day }
  getHours() { return localParts(this).hour }
  getMinutes() { return localParts(this).minute }
}

const { list, update, showError } = vi.hoisted(() => ({
  list: vi.fn(),
  update: vi.fn(),
  showError: vi.fn(),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    promo: {
      list,
      create: vi.fn(),
      update,
      delete: vi.fn(),
      getUsages: vi.fn(),
    },
  },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError,
    showSuccess: vi.fn(),
  }),
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({ copyToClipboard: vi.fn() }),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

const promo = (expiresAt: string | null) => ({
  id: 1,
  code: 'LOCAL-TIME',
  bonus_amount: 5,
  max_uses: 10,
  used_count: 0,
  status: 'active' as const,
  expires_at: expiresAt,
  notes: '',
  created_at: '2026-01-01T00:00:00Z',
})

const mountView = () => mount(PromoCodesView, {
  global: {
    stubs: {
      AppLayout: { template: '<div><slot /></div>' },
      TablePageLayout: { template: '<div><slot name="filters"/><slot name="table"/><slot name="pagination"/></div>' },
      DataTable: true,
      Pagination: true,
      ConfirmDialog: true,
      BaseDialog: { template: '<div><slot/><slot name="footer"/></div>' },
      Select: true,
      Icon: true,
    },
  },
})

describe('PromoCodesView edit expiry', () => {
  beforeEach(() => {
    vi.stubGlobal('Date', ZonedDate)
    list.mockReset().mockResolvedValue({ items: [], total: 0 })
    update.mockReset().mockResolvedValue({})
    showError.mockReset()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
  })

  it.each([
    ['winter', '2026-01-15T17:45:00.000Z'],
    ['summer', '2026-07-15T16:45:00.000Z'],
  ])('prefills %s expiry in local wall-clock time and round-trips the instant', async (_season, expiresAt) => {
    const wrapper = mountView()
    await flushPromises()

    ;(wrapper.vm as any).handleEdit(promo(expiresAt))
    await wrapper.vm.$nextTick()

    const value = (wrapper.vm as any).editForm.expires_at_str as string
    const source = new NativeDate(expiresAt)
    const parts = localParts(source)
    const expected = `${parts.year}-${String(parts.month).padStart(2, '0')}-${String(parts.day).padStart(2, '0')}T${String(parts.hour).padStart(2, '0')}:${String(parts.minute).padStart(2, '0')}`

    expect(value).toBe(expected)
    expect(Math.floor(new Date(value).getTime() / 60_000)).toBe(Math.floor(source.getTime() / 60_000))
  })

  it.each([null, 'not-a-date'])('safely leaves an empty expiry for %s', async (expiresAt) => {
    const wrapper = mountView()
    await flushPromises()

    expect(() => (wrapper.vm as any).handleEdit(promo(expiresAt))).not.toThrow()
    expect((wrapper.vm as any).editForm.expires_at_str).toBe('')
  })

  it('saves an unchanged value as the same minute-precision instant', async () => {
    const expiresAt = timezone === 'America/New_York'
      ? '2026-07-15T16:45:37.789Z'
      : '2026-07-15T04:45:37.789Z'
    const wrapper = mountView()
    await flushPromises()

    ;(wrapper.vm as any).handleEdit(promo(expiresAt))
    await (wrapper.vm as any).handleUpdate()

    expect(update).toHaveBeenCalledWith(1, expect.objectContaining({
      expires_at: Math.floor(new NativeDate(expiresAt).getTime() / 60_000) * 60,
    }))
  })

  it.each([
    ...(timezone === 'America/New_York' ? ['2026-03-08T02:30'] : []),
    '2026-01-15T12:34:56',
    'not-a-date',
  ])(
    'does not submit an invalid, nonexistent, or non-minute expiry value: %s',
    async (value) => {
      const wrapper = mountView()
      await flushPromises()
      ;(wrapper.vm as any).handleEdit(promo('2026-01-15T17:45:00.000Z'))
      ;(wrapper.vm as any).editForm.expires_at_str = value

      await (wrapper.vm as any).handleUpdate()

      expect(update).not.toHaveBeenCalled()
      expect(showError).toHaveBeenCalled()
    }
  )
})
