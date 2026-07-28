import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import SetupWizardView from '@/views/setup/SetupWizardView.vue'
import { install, testDatabase, testRedis } from '@/api/setup'

vi.mock('@/api/setup', () => ({
  install: vi.fn(),
  testDatabase: vi.fn(),
  testRedis: vi.fn()
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key })
}))

const global = {
  stubs: {
    Icon: true,
    Select: true,
    Toggle: true
  }
}

async function reachRedisStep(wrapper: ReturnType<typeof mount>) {
  vi.mocked(testDatabase).mockResolvedValue()
  await wrapper.findAll('button').find((button) => button.text().includes('setup.status.testConnection'))!.trigger('click')
  await flushPromises()
  await wrapper.findAll('button').find((button) => button.text().includes('common.next'))!.trigger('click')
}

describe('SetupWizardView Redis ACL username', () => {
  beforeEach(() => {
    vi.clearAllMocks()
  })

  it('passes special-character credentials unchanged when testing Redis', async () => {
    vi.mocked(testRedis).mockResolvedValue()
    const wrapper = mount(SetupWizardView, { global })
    await reachRedisStep(wrapper)

    const username = ' acl:user/@% '
    const password = ' password/@% '
    const usernameInput = wrapper.get('[data-testid="redis-username"]')
    const usernameHint = wrapper.get('#redis-username-hint')
    expect(wrapper.get('label[for="redis-username"]').exists()).toBe(true)
    expect(usernameInput.attributes('id')).toBe('redis-username')
    expect(usernameInput.attributes('aria-describedby')).toBe(usernameHint.attributes('id'))
    await usernameInput.setValue(username)
    await wrapper.get('[data-testid="redis-password"]').setValue(password)
    await wrapper.findAll('button').find((button) => button.text().includes('setup.status.testConnection'))!.trigger('click')
    await flushPromises()

    expect(testRedis).toHaveBeenCalledWith(expect.objectContaining({ username, password }))
  })

  it('sends an explicitly cleared username for default-user compatibility', async () => {
    vi.mocked(testRedis).mockResolvedValue()
    const wrapper = mount(SetupWizardView, { global })
    await reachRedisStep(wrapper)

    const input = wrapper.get('[data-testid="redis-username"]')
    await input.setValue('named-user')
    await input.setValue('')
    await wrapper.findAll('button').find((button) => button.text().includes('setup.status.testConnection'))!.trigger('click')
    await flushPromises()

    expect(testRedis).toHaveBeenCalledWith(expect.objectContaining({ username: '' }))
  })

  it('does not reveal Redis credentials in the ready summary or install response flow', async () => {
    vi.mocked(testDatabase).mockResolvedValue()
    vi.mocked(testRedis).mockResolvedValue()
    vi.mocked(install).mockRejectedValue(new Error('stop after payload capture'))
    const wrapper = mount(SetupWizardView, { global })
    await reachRedisStep(wrapper)

    const username = 'summary-secret-user/@%'
    const password = 'summary-secret-password/@%'
    await wrapper.get('[data-testid="redis-username"]').setValue(username)
    await wrapper.get('[data-testid="redis-password"]').setValue(password)
    await wrapper.findAll('button').find((button) => button.text().includes('setup.status.testConnection'))!.trigger('click')
    await flushPromises()
    await wrapper.findAll('button').find((button) => button.text().includes('common.next'))!.trigger('click')

    await wrapper.get('input[type="email"]').setValue('admin@example.com')
    const passwords = wrapper.findAll('input[type="password"]')
    await passwords[0].setValue('admin-password')
    await passwords[1].setValue('admin-password')
    await wrapper.findAll('button').find((button) => button.text().includes('common.next'))!.trigger('click')

    expect(wrapper.text()).not.toContain(username)
    expect(wrapper.text()).not.toContain(password)
    await wrapper.findAll('button').find((button) => button.text().includes('setup.status.completeInstallation'))!.trigger('click')
    await flushPromises()
    expect(install).toHaveBeenCalledWith(expect.objectContaining({
      redis: expect.objectContaining({ username, password })
    }))
  })
})
