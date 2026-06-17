import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { useGeoGateStore } from '@/stores/geoGate'

describe('useGeoGateStore', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.restoreAllMocks()
  })

  it('does nothing when disabled', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch')
    const store = useGeoGateStore()
    store.configure(false)

    await store.check(true)

    expect(fetchSpy).not.toHaveBeenCalled()
    expect(store.allowed).toBe(true)
    expect(store.blocked).toBe(false)
  })

  it('blocks CN responses', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true,
      json: async () => ({
        country_code: 'CN',
        country_name: 'China',
        city: 'Beijing',
      }),
    } as Response)

    const store = useGeoGateStore()
    store.configure(true)
    await store.check(true)

    expect(store.blocked).toBe(true)
    expect(store.countryCode).toBe('CN')
  })

  it('allows non-CN responses', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true,
      json: async () => ({
        country_code: 'US',
        country_name: 'United States',
      }),
    } as Response)

    const store = useGeoGateStore()
    store.configure(true)
    await store.check(true)

    expect(store.allowed).toBe(true)
    expect(store.blocked).toBe(false)
  })

  it('fails open on network errors', async () => {
    vi.spyOn(globalThis, 'fetch').mockRejectedValue(new Error('network'))

    const store = useGeoGateStore()
    store.configure(true)
    await store.check(true)

    expect(store.allowed).toBe(true)
    expect(store.blocked).toBe(false)
  })
})
