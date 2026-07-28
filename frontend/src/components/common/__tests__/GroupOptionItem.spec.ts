import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'

import GroupOptionItem from '../GroupOptionItem.vue'

describe('GroupOptionItem description', () => {
  it('preserves line breaks, wraps unbroken text, clamps three lines, and retains the full title', () => {
    const description = 'first line\nverylongunbrokenvalue-that-needs-anywhere-wrapping\nthird\nfourth'
    const wrapper = mount(GroupOptionItem, {
      props: { name: 'Group', platform: 'anthropic', description },
      global: { stubs: { GroupBadge: true } },
    })

    const text = wrapper.find('span.mt-1\\.5')
    expect(text.classes()).toEqual(expect.arrayContaining([
      'whitespace-pre-line', 'line-clamp-3', '[overflow-wrap:anywhere]',
    ]))
    expect(text.attributes('title')).toBe(description)
    expect(wrapper.findAll('span.mt-1\\.5')).toHaveLength(1)
  })
})
