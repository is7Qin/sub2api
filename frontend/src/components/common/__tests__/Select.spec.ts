import { mount, VueWrapper } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { nextTick } from 'vue'

import Select from '../Select.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

const originalWidth = window.innerWidth
const originalHeight = window.innerHeight
let wrapper: VueWrapper | undefined
let rect = { left: 20, top: 20, width: 80, height: 40 }

const setViewport = (width: number, height = 768) => {
  Object.defineProperty(window, 'innerWidth', { configurable: true, value: width })
  Object.defineProperty(window, 'innerHeight', { configurable: true, value: height })
}

const mockGeometry = (left: number, width: number, top = 20, height = 40) => {
  rect = { left, width, top, height }
  vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(() => ({
    x: rect.left, y: rect.top, left: rect.left, top: rect.top,
    right: rect.left + rect.width, bottom: rect.top + rect.height,
    width: rect.width, height: rect.height, toJSON: () => ({}),
  }))
}

const mountSelect = (props: Record<string, unknown> = {}) => {
  wrapper = mount(Select, {
    props: {
      modelValue: null,
      options: [
        { value: 'first', label: 'First' },
        { value: 'disabled', label: 'Disabled', disabled: true },
        { value: 'last', label: 'Last' },
      ],
      ...props,
    },
    attachTo: document.body,
  })
  return wrapper
}

const openSelect = async (props: Record<string, unknown> = {}) => {
  const mounted = mountSelect(props)
  await mounted.get('button').trigger('click')
  await nextTick()
  return document.body.querySelector<HTMLElement>('.select-dropdown-portal')!
}

const styleNumber = (element: HTMLElement, key: 'left' | 'minWidth' | 'maxWidth') =>
  Number.parseFloat(element.style[key])

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  document.body.innerHTML = ''
  setViewport(originalWidth, originalHeight)
  vi.restoreAllMocks()
})

describe('Select viewport geometry', () => {
  it.each([320, 375, 768, 1440])('keeps dropdown within 8px margins at %spx', async (viewport) => {
    setViewport(viewport)
    mockGeometry(viewport - 48, 40)
    const dropdown = await openSelect()
    const left = styleNumber(dropdown, 'left')
    const width = styleNumber(dropdown, 'maxWidth')
    expect(left).toBeGreaterThanOrEqual(8)
    expect(left + width).toBeLessThanOrEqual(viewport - 8)
    expect(styleNumber(dropdown, 'minWidth')).toBeGreaterThan(0)
    expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(document.documentElement.clientWidth)
  })

  it.each([
    ['normal narrow trigger', 1024, 20, 80, 20, 200],
    ['wide trigger', 1024, 20, 280, 20, 280],
    ['left-offscreen trigger', 320, -30, 80, 8, 200],
    ['right-edge trigger', 320, 280, 80, 112, 200],
    ['right-offscreen trigger', 320, 400, 80, 112, 200],
    ['tiny usable viewport', 20, 40, 80, 8, 4],
  ])('%s shifts or shrinks safely', async (_name, viewport, left, triggerWidth, expectedLeft, expectedWidth) => {
    setViewport(viewport)
    mockGeometry(left, triggerWidth)
    const dropdown = await openSelect()
    expect(styleNumber(dropdown, 'left')).toBe(expectedLeft)
    expect(styleNumber(dropdown, 'minWidth')).toBe(expectedWidth)
    expect(styleNumber(dropdown, 'maxWidth')).toBe(viewport - 8 - expectedLeft)
    expect(styleNumber(dropdown, 'minWidth')).toBeGreaterThan(0)
  })

  it('keeps geometry finite at a zero-width viewport', async () => {
    setViewport(0)
    mockGeometry(-20, 500)
    const dropdown = await openSelect()
    expect(dropdown.style.left).toBe('0px')
    expect(styleNumber(dropdown, 'minWidth')).toBeGreaterThan(0)
    expect(styleNumber(dropdown, 'maxWidth')).toBeGreaterThan(0)
  })

  it('recalculates geometry on resize and captured nested scroll', async () => {
    setViewport(375)
    mockGeometry(20, 80)
    const dropdown = await openSelect()
    expect(dropdown.style.left).toBe('20px')

    const scroller = document.createElement('div')
    const nested = document.createElement('div')
    scroller.appendChild(nested)
    document.body.appendChild(scroller)
    rect.left = 300
    nested.dispatchEvent(new Event('scroll'))
    await nextTick()
    expect(dropdown.style.left).toBe('167px')

    setViewport(320)
    window.dispatchEvent(new Event('resize'))
    await nextTick()
    expect(dropdown.style.left).toBe('112px')
  })
})

describe('Select interaction regression', () => {
  it('keeps Teleport/listbox keyboard selection and returns focus to trigger', async () => {
    mockGeometry(20, 200)
    const mounted = mountSelect()
    const trigger = mounted.get('button')
    await trigger.trigger('keydown', { key: 'ArrowDown' })
    await nextTick()
    const listbox = document.body.querySelector<HTMLElement>('[role="listbox"]')!
    expect(listbox).not.toBeNull()
    listbox.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }))
    listbox.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }))
    await nextTick()
    expect(mounted.emitted('update:modelValue')?.[0]).toEqual(['last'])
    expect(document.activeElement).toBe(trigger.element)
  })

  it('keeps search, clear, and disabled behavior', async () => {
    mockGeometry(20, 200)
    const searchable = await openSelect({ searchable: true, clearable: true, modelValue: 'first' })
    const input = searchable.querySelector<HTMLInputElement>('input')!
    input.value = 'Last'
    input.dispatchEvent(new Event('input', { bubbles: true }))
    await nextTick()
    expect(searchable.textContent).toContain('Last')
    expect(searchable.textContent).not.toContain('First')

    await wrapper!.get('.select-clear').trigger('click')
    expect(wrapper!.emitted('update:modelValue')?.at(-1)).toEqual([null])
    expect(wrapper!.emitted('change')?.at(-1)).toEqual([null, null])

    wrapper?.unmount()
    wrapper = undefined
    document.body.innerHTML = ''
    const disabled = mountSelect({ disabled: true })
    await disabled.get('button').trigger('click')
    expect(document.body.querySelector('[role="listbox"]')).toBeNull()
  })
})
