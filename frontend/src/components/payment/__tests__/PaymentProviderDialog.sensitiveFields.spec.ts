import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import PaymentProviderDialog from '@/components/payment/PaymentProviderDialog.vue'
import en from '@/i18n/locales/en'
import zh from '@/i18n/locales/zh'

const localeState = vi.hoisted(() => ({
  locale: 'en' as 'en' | 'zh',
  messages: {} as Record<string, Record<string, unknown>>,
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => {
      const value = key.split('.').reduce<unknown>(
        (current, segment) =>
          current && typeof current === 'object'
            ? (current as Record<string, unknown>)[segment]
            : undefined,
        localeState.messages[localeState.locale],
      )
      return typeof value === 'string' ? value : key
    },
  }),
}))

function mountDialog(locale: 'en' | 'zh') {
  localeState.locale = locale
  localeState.messages = { en, zh }

  return mount(PaymentProviderDialog, {
    props: {
      show: true,
      saving: false,
      editing: null,
      allKeyOptions: [],
      enabledKeyOptions: [],
      allPaymentTypes: [],
      redirectLabel: 'Redirect',
    },
    global: {
      stubs: {
        BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' },
        Select: { template: '<div />' },
        ToggleSwitch: { template: '<div />' },
      },
    },
  })
}

describe('PaymentProviderDialog sensitive value toggles', () => {
  it.each([
    ['en', 'Show sensitive value', 'Hide sensitive value'],
    ['zh', '显示敏感值', '隐藏敏感值'],
  ] as const)('localizes and updates sensitive value toggle labels in %s', async (locale, showLabel, hideLabel) => {
    const wrapper = mountDialog(locale)

    // These are all providers with sensitive inputs rendered as visibility toggles;
    // key-shaped credentials use multiline password-manager-safe textareas instead.
    for (const providerKey of ['easypay', 'stripe', 'airwallex']) {
      ;(wrapper.vm as unknown as { reset: (key: string) => void }).reset(providerKey)
      await nextTick()

      const toggles = wrapper.findAll(`button[aria-label="${showLabel}"]`)
      expect(toggles).toHaveLength(1)
      const toggle = toggles[0]
      const sensitiveInput = toggle.element.parentElement?.querySelector('input')
      if (!sensitiveInput) throw new Error(`sensitive input not found for ${providerKey}`)

      expect(toggle.attributes('type')).toBe('button')
      expect(toggle.find('svg').exists()).toBe(true)
      expect(sensitiveInput.getAttribute('type')).toBe('password')
      await toggle.trigger('click')

      expect(sensitiveInput.getAttribute('type')).toBe('text')
      expect(wrapper.get(`button[aria-label="${hideLabel}"]`).element).toBe(toggle.element)
    }
  })
})
