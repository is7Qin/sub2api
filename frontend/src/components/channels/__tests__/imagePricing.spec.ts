import { afterEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import type { UserAvailableChannel, UserSupportedModel } from '@/api/channels'

const messages: Record<string, string> = {
  'availableChannels.modelCount': '1 model',
  'availableChannels.modelSearchPlaceholder': 'Search models',
  'availableChannels.perMillionHint': 'Token prices per million',
  'availableChannels.modelColumn': 'Model',
  'availableChannels.billingColumn': 'Billing',
  'availableChannels.priceInput': 'Input',
  'availableChannels.priceOutput': 'Output',
  'availableChannels.priceCacheRead': 'Cache read',
  'availableChannels.priceCacheWrite': 'Cache write',
  'availableChannels.pricePerRequest': 'Per req',
  'availableChannels.priceImage': 'Per image',
  'availableChannels.pricing.billingMode': 'Billing mode',
  'availableChannels.pricing.billingModeImage': 'Image',
  'availableChannels.pricing.imageOutputPrice': 'Image output',
  'availableChannels.pricing.perRequestPrice': 'Per request',
  'availableChannels.pricing.unitPerRequest': '/ request',
}

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => messages[key] ?? key,
  }),
}))

import AvailableChannelsTable from '../AvailableChannelsTable.vue'
import SupportedModelChip from '../SupportedModelChip.vue'

const imageModel: UserSupportedModel = {
  name: 'image-model',
  platform: 'openai',
  pricing: {
    billing_mode: 'image',
    input_price: null,
    output_price: null,
    cache_write_price: null,
    cache_read_price: null,
    image_output_price: 0.00003,
    per_request_price: 0.08,
    intervals: [],
  },
}

const imageChannel: UserAvailableChannel = {
  name: 'Image channel',
  description: 'Image generation',
  platforms: [
    {
      platform: 'openai',
      groups: [],
      supported_models: [imageModel],
    },
  ],
}

describe('available-channel image pricing', () => {
  afterEach(() => {
    document.body.innerHTML = ''
  })

  it('renders the per-request price in AvailableChannelsTable', () => {
    const wrapper = mount(AvailableChannelsTable, {
      props: {
        columns: {
          name: 'Name',
          description: 'Description',
          platform: 'Platform',
          groups: 'Groups',
          supportedModels: 'Models',
        },
        rows: [imageChannel],
        loading: false,
        pricingKeyPrefix: 'availableChannels.pricing',
        noPricingLabel: 'No pricing',
        noModelsLabel: 'No models',
        emptyLabel: 'No channels',
        userGroupRates: {},
      },
      global: {
        stubs: {
          Icon: true,
          PlatformIcon: true,
          GroupBadge: true,
        },
      },
    })

    expect(wrapper.text()).toContain('$0.08 / Per req')
    expect(wrapper.text()).not.toContain('$0.00003')
  })

  it('renders the per-request price in SupportedModelChip image pricing popover', async () => {
    const wrapper = mount(SupportedModelChip, {
      attachTo: document.body,
      props: {
        model: imageModel,
      },
      global: {
        stubs: {
          PlatformIcon: true,
        },
      },
    })

    await wrapper.get('[tabindex="0"]').trigger('mouseenter')
    await nextTick()

    const popover = document.body.querySelector('[role="tooltip"]')
    if (!(popover instanceof HTMLElement)) {
      throw new Error('pricing popover not found')
    }

    expect(popover.textContent).toContain('Per request')
    expect(popover.textContent).toContain('$0.08 / request')
    expect(popover.textContent).not.toContain('$0.00003')

    wrapper.unmount()
  })
})
