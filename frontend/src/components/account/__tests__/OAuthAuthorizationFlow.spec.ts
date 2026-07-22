import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import OAuthAuthorizationFlow from '../OAuthAuthorizationFlow.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key })
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({ copied: false, copyToClipboard: vi.fn() })
}))

describe('OAuthAuthorizationFlow Agent Identity import', () => {
  it('exposes a reachable Agent Identity form and submits its content', async () => {
    const wrapper = mount(OAuthAuthorizationFlow, {
      props: {
        addMethod: 'oauth',
        platform: 'openai',
        showCookieOption: false,
        showCodexSessionImportOption: true,
        showAgentIdentityImportOption: true
      },
      global: { stubs: { Icon: true } }
    })

    const option = wrapper.get('input[value="agent_identity"]')
    await option.setValue(true)

    const input = wrapper.get('textarea[placeholder="admin.accounts.oauth.openai.agentIdentityPlaceholder"]')
    const authJSON = '{"auth_mode":"agentIdentity","agent_runtime_id":"runtime"}'
    await input.setValue(authJSON)
    await wrapper.get('button.btn-primary.w-full').trigger('click')

    expect(wrapper.emitted('update:inputMethod')?.at(-1)).toEqual(['agent_identity'])
    expect(wrapper.emitted('import-codex-session')?.at(-1)).toEqual([authJSON])
  })
})
