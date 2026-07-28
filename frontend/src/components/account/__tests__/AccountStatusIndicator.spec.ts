import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import AccountStatusIndicator from '../AccountStatusIndicator.vue'
import { i18n } from '@/i18n'
import enLocale from '@/i18n/locales/en'
import type { Account } from '@/types'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  const lookup = (key: string): unknown => key.split('.').reduce<unknown>((value, segment) =>
    value && typeof value === 'object' ? (value as Record<string, unknown>)[segment] : undefined, enLocale)
  const interpolate = (message: string, params?: Record<string, unknown>) =>
    message.replace(/\{(\w+)\}/g, (_, name: string) => String(params?.[name] ?? `{${name}}`))
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) => {
        const message = lookup(key)
        return typeof message === 'string' ? interpolate(message, params) : key
      }
    })
  }
})

function makeAccount(overrides: Partial<Account>): Account {
  return {
    id: 1,
    name: 'account',
    platform: 'antigravity',
    type: 'oauth',
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    status: 'active',
    error_message: null,
    last_used_at: null,
    expires_at: null,
    auto_pause_on_expired: true,
    created_at: '2026-03-15T00:00:00Z',
    updated_at: '2026-03-15T00:00:00Z',
    schedulable: true,
    rate_limited_at: null,
    rate_limit_reset_at: null,
    overload_until: null,
    temp_unschedulable_until: null,
    temp_unschedulable_reason: null,
    session_window_start: null,
    session_window_end: null,
    session_window_status: null,
    ...overrides,
  }
}

describe('AccountStatusIndicator', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-03-15T00:00:00Z'))
    i18n.global.setLocaleMessage('en', {
      ...enLocale,
      common: {
        ...enLocale.common,
        time: {
          ...enLocale.common.time,
          countdown: {
            daysHours: ({ named }: any) => `${named('d')}d ${named('h')}h`,
            hoursMinutes: ({ named }: any) => `${named('h')}h ${named('m')}m`,
            minutes: ({ named }: any) => `${named('m')}m`,
            withSuffix: ({ named }: any) => `${named('time')} to lift`,
          },
        },
      },
    })
    i18n.global.locale.value = 'en'
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('模型限流 + overages 启用 + 无 AICredits key → 显示 ⚡ (credits_active)', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          id: 1,
          name: 'ag-1',
          extra: {
            allow_overages: true,
            model_rate_limits: {
              'claude-sonnet-4-5': {
                rate_limited_at: '2026-03-15T00:00:00Z',
                rate_limit_reset_at: '2099-03-15T00:00:00Z'
              }
            }
          }
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    expect(wrapper.text()).toContain('⚡')
    expect(wrapper.text()).toContain('CSon45')
  })

  it('模型限流 + overages 未启用 → 普通限流样式（无 ⚡）', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          id: 2,
          name: 'ag-2',
          extra: {
            model_rate_limits: {
              'claude-sonnet-4-5': {
                rate_limited_at: '2026-03-15T00:00:00Z',
                rate_limit_reset_at: '2099-03-15T00:00:00Z'
              }
            }
          }
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    expect(wrapper.text()).toContain('CSon45')
    expect(wrapper.text()).not.toContain('⚡')
  })

  it('AICredits key 生效 → 显示积分已用尽 (credits_exhausted)', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          id: 3,
          name: 'ag-3',
          extra: {
            allow_overages: true,
            model_rate_limits: {
              'AICredits': {
                rate_limited_at: '2026-03-15T00:00:00Z',
                rate_limit_reset_at: '2099-03-15T00:00:00Z'
              }
            }
          }
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    expect(wrapper.text()).toContain('Credits Exhausted')
  })

  it('模型限流 + overages 启用 + AICredits key 生效 → 普通限流样式（积分耗尽，无 ⚡）', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          id: 4,
          name: 'ag-4',
          extra: {
            allow_overages: true,
            model_rate_limits: {
              'claude-sonnet-4-5': {
                rate_limited_at: '2026-03-15T00:00:00Z',
                rate_limit_reset_at: '2099-03-15T00:00:00Z'
              },
              'AICredits': {
                rate_limited_at: '2026-03-15T00:00:00Z',
                rate_limit_reset_at: '2099-03-15T00:00:00Z'
              }
            }
          }
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    // 模型限流 + 积分耗尽 → 不应显示 ⚡
    expect(wrapper.text()).toContain('CSon45')
    expect(wrapper.text()).not.toContain('⚡')
    // AICredits 积分耗尽状态应显示
    expect(wrapper.text()).toContain('Credits Exhausted')
  })

  it.each([
    ['credits_active', true, { 'claude-sonnet-4-5': { rate_limited_at: '2026-03-15T00:00:00Z', rate_limit_reset_at: '2026-03-16T06:15:00Z' } }],
    ['rate_limit', false, { 'claude-sonnet-4-5': { rate_limited_at: '2026-03-15T00:00:00Z', rate_limit_reset_at: '2026-03-16T06:15:00Z' } }],
    ['credits_exhausted', true, { AICredits: { rate_limited_at: '2026-03-15T00:00:00Z', rate_limit_reset_at: '2026-03-16T06:15:00Z' } }],
  ])('uses shared day-aware countdown and full-date tooltip for %s', (_kind, allowOverages, limits) => {
    const wrapper = mount(AccountStatusIndicator, {
      props: { account: makeAccount({ extra: { allow_overages: allowOverages, model_rate_limits: limits } }) },
      global: { stubs: { Icon: true } },
    })

    expect(wrapper.text()).toContain('1d 6h')
    expect(wrapper.text()).not.toContain('30h15m')
    const tooltip = wrapper.find('[role="tooltip"]')
    expect(tooltip.exists()).toBe(true)
    expect(tooltip.text()).toContain('2026')
  })

  it('makes model reset details discoverable by focus and hover within viewport-safe wrapping', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: { account: makeAccount({ extra: { model_rate_limits: {
        'claude-sonnet-4-5': { rate_limited_at: '2026-03-15T00:00:00Z', rate_limit_reset_at: '2026-03-16T06:15:00Z' },
      } } }) },
      global: { stubs: { Icon: true } },
    })

    const trigger = wrapper.find('[aria-describedby]')
    const tooltip = wrapper.find('[role="tooltip"]')
    expect(trigger.attributes('tabindex')).toBe('0')
    expect(trigger.attributes('aria-describedby')).toBe(tooltip.attributes('id'))
    expect(trigger.classes()).toContain('group')
    expect(tooltip.classes()).toContain('group-focus-within:opacity-100')
    expect(tooltip.classes()).toContain('group-hover:opacity-100')
    expect(tooltip.classes()).toContain('max-w-[calc(100vw-16px)]')
    expect(tooltip.classes()).toContain('whitespace-normal')
    expect(tooltip.classes()).toContain('break-words')
    expect(tooltip.classes()).toContain('fixed')
    expect(tooltip.classes()).not.toContain('sm:absolute')
  })

  it('uses stable unique tooltip IDs even when model names sanitize alike', () => {
    const account = makeAccount({ id: 7, extra: { model_rate_limits: {
      'model.a': { rate_limited_at: '2026-03-15T00:00:00Z', rate_limit_reset_at: '2026-03-16T06:15:00Z' },
      'model/a': { rate_limited_at: '2026-03-15T00:00:00Z', rate_limit_reset_at: '2026-03-16T06:15:00Z' },
    } } })
    const wrapper = mount(AccountStatusIndicator, {
      props: { account },
      global: { stubs: { Icon: true } },
    })

    const ids = wrapper.findAll('[role="tooltip"]').map((tooltip) => tooltip.attributes('id'))
    expect(new Set(ids).size).toBe(2)

    wrapper.setProps({ account: { ...account, name: 'renamed' } })
    expect(wrapper.findAll('[role="tooltip"]').map((tooltip) => tooltip.attributes('id'))).toEqual(ids)
  })

  it('leaves top-level 429 and 529 behavior unchanged', () => {
    const rateLimited = mount(AccountStatusIndicator, {
      props: { account: makeAccount({ rate_limit_reset_at: '2026-03-15T01:00:00Z' }) },
      global: { stubs: { Icon: true } },
    })
    expect(rateLimited.text()).toContain('429')

    const overloaded = mount(AccountStatusIndicator, {
      props: { account: makeAccount({ overload_until: '2026-03-15T01:00:00Z' }) },
      global: { stubs: { Icon: true } },
    })
    expect(overloaded.text()).toContain('529')
  })
})
