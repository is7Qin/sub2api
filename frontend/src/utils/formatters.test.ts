import { describe, expect, it } from 'vitest'
import { formatMultiplier } from './formatters'

describe('formatMultiplier', () => {
  it('preserves meaningful multiplier precision', () => {
    expect(formatMultiplier(0.035)).toBe('0.035')
    expect(formatMultiplier(0.0625)).toBe('0.0625')
    expect(formatMultiplier(0.3)).toBe('0.30')
    expect(formatMultiplier(1)).toBe('1.00')
  })
})
