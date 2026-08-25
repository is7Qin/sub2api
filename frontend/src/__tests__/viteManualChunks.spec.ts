// @vitest-environment node

import { describe, expect, it } from 'vitest'
import type { ConfigEnv, UserConfig } from 'vite'
import viteConfig from '../../vite.config'

function resolveBuildConfig(): UserConfig {
  return (viteConfig as (env: ConfigEnv) => UserConfig)({
    command: 'build',
    mode: 'production',
    isSsrBuild: false,
    isPreview: false
  })
}

describe('Vite manual chunks', () => {
  it('keeps Stripe outside the eagerly loaded miscellaneous vendor chunk', () => {
    const config = resolveBuildConfig()
    const manualChunks = config.build?.rollupOptions?.output?.manualChunks

    expect(typeof manualChunks).toBe('function')
    expect(manualChunks!('C:/project/frontend/node_modules/@stripe/stripe-js/dist/index.mjs')).toBe('vendor-stripe')
    expect(manualChunks!('C:/project/frontend/node_modules/axios/lib/axios.js')).toBe('vendor-misc')
  })
})
