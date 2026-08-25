import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'

const {
  createAccountMock,
  createOpenAIAccountFromRefreshTokenMock,
  checkMixedChannelRiskMock,
  refreshOpenAITokenMock,
  importCodexSessionMock,
  getWebSearchEmulationConfigMock,
  getSettingsMock,
  listTLSFingerprintProfilesMock
} = vi.hoisted(() => ({
  createAccountMock: vi.fn(),
  createOpenAIAccountFromRefreshTokenMock: vi.fn(),
  checkMixedChannelRiskMock: vi.fn(),
  refreshOpenAITokenMock: vi.fn(),
  importCodexSessionMock: vi.fn(),
  getWebSearchEmulationConfigMock: vi.fn(),
  getSettingsMock: vi.fn(),
  listTLSFingerprintProfilesMock: vi.fn()
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
    showWarning: vi.fn(),
    showInfo: vi.fn()
  })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    isSimpleMode: true
  })
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      create: createAccountMock,
      createOpenAIAccountFromRefreshToken: createOpenAIAccountFromRefreshTokenMock,
      checkMixedChannelRisk: checkMixedChannelRiskMock,
      refreshOpenAIToken: refreshOpenAITokenMock,
      generateAuthUrl: vi.fn(),
      exchangeCode: vi.fn(),
      importCodexSession: importCodexSessionMock
    },
    settings: {
      getWebSearchEmulationConfig: getWebSearchEmulationConfigMock,
      getSettings: getSettingsMock
    },
    tlsFingerprintProfiles: {
      list: listTLSFingerprintProfilesMock
    }
  }
}))

vi.mock('@/api/admin/accounts', () => ({
  getAntigravityDefaultModelMapping: vi.fn()
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

import CreateAccountModal from '../CreateAccountModal.vue'

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: {
    show: {
      type: Boolean,
      default: false
    }
  },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

const SelectStub = defineComponent({
  name: 'SelectStub',
  props: {
    modelValue: {
      type: [String, Number, Boolean, null],
      default: ''
    },
    options: {
      type: Array,
      default: () => []
    }
  },
  emits: ['update:modelValue'],
  template: `
    <select
      v-bind="$attrs"
      :value="modelValue"
      @change="$emit('update:modelValue', $event.target.value)"
    >
      <option v-for="option in options" :key="option.value" :value="option.value">
        {{ option.label }}
      </option>
    </select>
  `
})

const OAuthAuthorizationFlowStub = defineComponent({
  name: 'OAuthAuthorizationFlow',
  emits: ['validate-refresh-token', 'import-personal-access-token', 'import-codex-session'],
  setup(_, { emit }) {
    const emitCodexSession = () => emit('import-codex-session', '{"personal_access_token":"pat-test"}')
    return { emitCodexSession }
  },
  template: `
    <div data-testid="oauth-flow">
      <button
        type="button"
        data-testid="emit-refresh-token"
        @click="$emit('validate-refresh-token', 'rt-test')"
      >
        validate refresh token
      </button>
      <button
        type="button"
        data-testid="emit-personal-access-token"
        @click="$emit('import-personal-access-token', 'pat-direct')"
      >
        import personal access token
      </button>
      <button
        type="button"
        data-testid="emit-codex-session"
        @click="emitCodexSession"
      >
        import codex session
      </button>
    </div>
  `
})

function mountModal() {
  return mount(CreateAccountModal, {
    props: {
      show: true,
      proxies: [],
      groups: []
    },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        ConfirmDialog: true,
        Select: SelectStub,
        Icon: true,
        ProxySelector: true,
        ProxyAdBanner: true,
        GroupSelector: true,
        ModelWhitelistSelector: true,
        QuotaLimitCard: true,
        OAuthAuthorizationFlow: OAuthAuthorizationFlowStub
      }
    }
  })
}

describe('CreateAccountModal', () => {
  beforeEach(() => {
    createAccountMock.mockReset()
    createOpenAIAccountFromRefreshTokenMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    refreshOpenAITokenMock.mockReset()
    importCodexSessionMock.mockReset()
    getWebSearchEmulationConfigMock.mockReset()
    getSettingsMock.mockReset()
    listTLSFingerprintProfilesMock.mockReset()

    createAccountMock.mockResolvedValue({})
    createOpenAIAccountFromRefreshTokenMock.mockResolvedValue({})
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
    refreshOpenAITokenMock.mockResolvedValue({
      access_token: 'redacted-access-token',
      refresh_token: 'redacted-refresh-token',
      expires_at: 1790000000,
      email: 'openai@example.com',
      name: 'OpenAI User',
      privacy_mode: 'training_disabled'
    })
    importCodexSessionMock.mockResolvedValue({
      total: 1,
      created: 1,
      updated: 0,
      skipped: 0,
      failed: 0
    })
    getWebSearchEmulationConfigMock.mockResolvedValue({ enabled: false, providers: [] })
    getSettingsMock.mockResolvedValue({})
    listTLSFingerprintProfilesMock.mockResolvedValue([])
  })

  it('creates OpenAI OAuth accounts with OAuth-safe WS mode and no passthrough keys', async () => {
    const wrapper = mountModal()

    await wrapper.get('input[data-tour="account-form-name"]').setValue('OpenAI OAuth')
    await wrapper.get('[data-testid="create-platform-openai"]').trigger('click')
    await wrapper.get('[data-testid="create-openai-oauth-type"]').trigger('click')
    await flushPromises()

    expect(wrapper.text()).not.toContain('admin.accounts.openai.apiKeyPassthrough')
    const wsModeSelect = wrapper.get('[data-testid="create-openai-ws-mode-select"]')
    expect(wsModeSelect.find('option[value="managed_session"]').exists()).toBe(true)
    expect(wsModeSelect.find('option[value="passthrough"]').exists()).toBe(false)
    expect(wsModeSelect.find('option[value="ctx_pool"]').exists()).toBe(false)

    await wsModeSelect.setValue('managed_session')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()
    await wrapper.get('[data-testid="emit-refresh-token"]').trigger('click')
    await flushPromises()

    expect(createAccountMock).not.toHaveBeenCalled()
    expect(refreshOpenAITokenMock).not.toHaveBeenCalled()
    expect(createOpenAIAccountFromRefreshTokenMock).toHaveBeenCalledTimes(1)
    const payload = createOpenAIAccountFromRefreshTokenMock.mock.calls[0]?.[0]
    expect(payload).toMatchObject({
      refresh_token: 'rt-test',
      name: 'OpenAI OAuth',
      extra: expect.objectContaining({
        openai_oauth_ws_mode: 'managed_session'
      })
    })
    expect(payload.extra).not.toHaveProperty('openai_passthrough')
    expect(payload.extra).not.toHaveProperty('openai_oauth_passthrough')
    expect(payload.extra).not.toHaveProperty('openai_oauth_responses_websockets_v2_mode')
    expect(payload.extra).not.toHaveProperty('openai_oauth_responses_websockets_v2_enabled')
    expect(payload.extra).not.toHaveProperty('openai_apikey_responses_websockets_v2_mode')
    expect(payload.extra).not.toHaveProperty('openai_apikey_responses_websockets_v2_enabled')
  })

  it('shows and submits the Codex namespace flatten toggle only for OpenAI OAuth', async () => {
    const wrapper = mountModal()

    await wrapper.get('input[data-tour="account-form-name"]').setValue('OpenAI OAuth')
    await wrapper.get('[data-testid="create-platform-openai"]').trigger('click')
    await wrapper.get('[data-testid="create-openai-oauth-type"]').trigger('click')
    await flushPromises()

    const toggle = wrapper.get('[data-testid="create-openai-flatten-namespaces-toggle"]')
    await toggle.trigger('click')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()
    await wrapper.get('[data-testid="emit-refresh-token"]').trigger('click')
    await flushPromises()

    const payload = createOpenAIAccountFromRefreshTokenMock.mock.calls[0]?.[0]
    expect(payload.extra?.openai_responses_flatten_namespaces).toBe(true)

    const apiKeyWrapper = mountModal()
    await apiKeyWrapper.get('[data-testid="create-platform-openai"]').trigger('click')
    await apiKeyWrapper.get('[data-testid="create-openai-apikey-type"]').trigger('click')
    expect(apiKeyWrapper.find('[data-testid="create-openai-flatten-namespaces-toggle"]').exists()).toBe(false)
  })

  it('imports OpenAI Codex PAT JSON through the backend import endpoint', async () => {
    const wrapper = mountModal()

    await wrapper.get('input[data-tour="account-form-name"]').setValue('OpenAI PAT')
    await wrapper.get('[data-testid="create-platform-openai"]').trigger('click')
    await wrapper.get('[data-testid="create-openai-oauth-type"]').trigger('click')
    await flushPromises()

    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()
    await wrapper.get('[data-testid="emit-codex-session"]').trigger('click')
    await flushPromises()

    expect(importCodexSessionMock).toHaveBeenCalledTimes(1)
    const payload = importCodexSessionMock.mock.calls[0]?.[0]
    expect(payload).toMatchObject({
      content: '{"personal_access_token":"pat-test"}',
      name: 'OpenAI PAT',
      update_existing: true,
      extra: expect.objectContaining({
        openai_oauth_ws_mode: 'off'
      })
    })
    expect(payload.credential_extras).not.toHaveProperty('personal_access_token')
  })

  it('imports OpenAI PAT direct input through the backend import endpoint', async () => {
    const wrapper = mountModal()

    await wrapper.get('input[data-tour="account-form-name"]').setValue('OpenAI PAT Direct')
    await wrapper.get('[data-testid="create-platform-openai"]').trigger('click')
    await wrapper.get('[data-testid="create-openai-oauth-type"]').trigger('click')
    await flushPromises()

    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()
    await wrapper.get('[data-testid="emit-personal-access-token"]').trigger('click')
    await flushPromises()

    expect(importCodexSessionMock).toHaveBeenCalledTimes(1)
    const payload = importCodexSessionMock.mock.calls[0]?.[0]
    expect(payload).toMatchObject({
      content: '{"personal_access_token":"pat-direct"}',
      name: 'OpenAI PAT Direct',
      update_existing: true,
      extra: expect.objectContaining({
        openai_oauth_ws_mode: 'off'
      })
    })
    expect(payload.credential_extras).not.toHaveProperty('personal_access_token')
  })

  it('preserves OpenAI APIKey passthrough and APIKey WS payload fields', async () => {
    const wrapper = mountModal()

    await wrapper.get('input[data-tour="account-form-name"]').setValue('OpenAI APIKey')
    await wrapper.get('[data-testid="create-platform-openai"]').trigger('click')
    await wrapper.get('[data-testid="create-openai-apikey-type"]').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('admin.accounts.openai.apiKeyPassthrough')
    const wsModeSelect = wrapper.get('[data-testid="create-openai-ws-mode-select"]')
    expect(wsModeSelect.find('option[value="passthrough"]').exists()).toBe(true)
    expect(wsModeSelect.find('option[value="ctx_pool"]').exists()).toBe(true)
    expect(wsModeSelect.find('option[value="managed_session"]').exists()).toBe(false)

    await wrapper.get('[data-testid="create-api-key-input"]').setValue('sk-test')
    await wrapper.get('[data-testid="create-openai-passthrough-toggle"]').trigger('click')
    await wsModeSelect.setValue('passthrough')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    const payload = createAccountMock.mock.calls[0]?.[0]
    expect(payload).toMatchObject({
      name: 'OpenAI APIKey',
      platform: 'openai',
      type: 'apikey',
      credentials: expect.objectContaining({
        api_key: 'sk-test',
        base_url: 'https://api.openai.com'
      }),
      extra: {
        openai_apikey_responses_websockets_v2_mode: 'passthrough',
        openai_apikey_responses_websockets_v2_enabled: true,
        openai_passthrough: true
      }
    })
    expect(payload.extra).not.toHaveProperty('openai_oauth_ws_mode')
    expect(payload.extra).not.toHaveProperty('openai_oauth_passthrough')
  })
})
