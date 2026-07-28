import { beforeEach, describe, expect, it, vi } from 'vitest'

const { put } = vi.hoisted(() => ({
  put: vi.fn()
}))

vi.mock('@/api/client', () => ({
  apiClient: {
    put
  }
}))

import { updateS3Config } from '@/api/admin/backup'

describe('admin backup API', () => {
  beforeEach(() => {
    put.mockReset()
    put.mockResolvedValue({ data: {} })
  })

  it('sends the TOTP code with an S3 configuration update', async () => {
    const config = {
      endpoint: 'https://s3.example.com',
      region: 'us-east-1',
      bucket: 'backups',
      access_key_id: 'access-key',
      secret_access_key: 'secret-key',
      prefix: 'daily/',
      force_path_style: true,
      totp_code: '123456'
    }

    await updateS3Config(config)

    expect(put).toHaveBeenCalledWith('/admin/backups/s3-config', config)
  })
})
