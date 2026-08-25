import { beforeEach, describe, expect, it, vi } from 'vitest'

const { post, put } = vi.hoisted(() => ({ post: vi.fn(), put: vi.fn() }))

vi.mock('@/api/client', () => ({ apiClient: { post, put } }))

import { create, update } from '@/api/keys'
import { updateApiKey } from '@/api/admin/apiKeys'

describe('API key concurrency serialization', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    post.mockResolvedValue({ data: {} })
    put.mockResolvedValue({ data: {} })
  })

  it('includes the create default of zero', async () => {
    await create('key', 1, undefined, [], [], 0, undefined, undefined, { concurrency: 0 })
    expect(post).toHaveBeenCalledWith('/keys', expect.objectContaining({ concurrency: 0 }))
  })

  it('preserves explicit zero and omission on user updates', async () => {
    await update(7, { concurrency: 0 })
    await update(7, { name: 'renamed' })
    expect(put).toHaveBeenNthCalledWith(1, '/keys/7', { concurrency: 0 })
    expect(put).toHaveBeenNthCalledWith(2, '/keys/7', { name: 'renamed' })
  })

  it('sends admin concurrency updates without unrelated fields', async () => {
    await updateApiKey(9, { concurrency: 12 })
    expect(put).toHaveBeenCalledWith('/admin/api-keys/9', { concurrency: 12 })
  })
})
