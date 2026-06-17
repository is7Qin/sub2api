import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

const GEO_LOOKUP_URL = 'https://ipapi.co/json/'
const BLOCKED_COUNTRY_CODE = 'CN'

export interface GeoGateResult {
  countryCode: string
  countryName: string
  city: string
}

type GeoGateState = 'idle' | 'checking' | 'allowed' | 'blocked' | 'error'

function normalizeString(value: unknown): string {
  return typeof value === 'string' ? value.trim() : ''
}

function buildCountryLabel(countryCode: string, countryName: string): string {
  if (countryName) {
    return countryName
  }
  return countryCode || ''
}

export const useGeoGateStore = defineStore('geoGate', () => {
  const enabled = ref(false)
  const state = ref<GeoGateState>('idle')
  const countryCode = ref('')
  const countryName = ref('')
  const city = ref('')
  const message = ref('')

  const checking = computed(() => state.value === 'checking')
  const blocked = computed(() => state.value === 'blocked')
  const allowed = computed(() => state.value === 'allowed')

  function configure(isEnabled: boolean): void {
    enabled.value = isEnabled
    if (!isEnabled) {
      state.value = 'allowed'
      countryCode.value = ''
      countryName.value = ''
      city.value = ''
      message.value = ''
    }
  }

  async function check(force = false): Promise<void> {
    if (!enabled.value) {
      state.value = 'allowed'
      return
    }
    if (checking.value) {
      return
    }
    if (!force && (state.value === 'allowed' || state.value === 'blocked')) {
      return
    }

    state.value = 'checking'
    message.value = ''

    try {
      const response = await fetch(GEO_LOOKUP_URL, {
        method: 'GET',
        cache: 'no-store',
        headers: {
          Accept: 'application/json',
        },
      })
      if (!response.ok) {
        throw new Error(`geo lookup failed: ${response.status}`)
      }

      const payload = await response.json() as Record<string, unknown>
      const nextCountryCode = normalizeString(payload.country_code).toUpperCase()
      const nextCountryName = normalizeString(payload.country_name)
      const nextCity = normalizeString(payload.city)

      countryCode.value = nextCountryCode
      countryName.value = nextCountryName
      city.value = nextCity

      if (nextCountryCode === BLOCKED_COUNTRY_CODE) {
        state.value = 'blocked'
        message.value = buildCountryLabel(nextCountryCode, nextCountryName)
        return
      }

      state.value = 'allowed'
      message.value = buildCountryLabel(nextCountryCode, nextCountryName)
    } catch {
      // 初步推行：识别失败直接放行
      state.value = 'allowed'
      message.value = ''
    }
  }

  async function recheck(): Promise<void> {
    await check(true)
  }

  return {
    enabled,
    state,
    checking,
    blocked,
    allowed,
    countryCode,
    countryName,
    city,
    message,
    configure,
    check,
    recheck,
  }
})
