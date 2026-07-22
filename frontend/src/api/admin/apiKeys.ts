/**
 * Admin API Keys API endpoints
 * Handles API key management for administrators
 */

import { apiClient } from '../client'
import type { ApiKey } from '@/types'

export interface UpdateApiKeyResult {
  api_key: ApiKey
  auto_granted_group_access: boolean
  granted_group_id?: number
  granted_group_name?: string
}

export interface AdminUpdateApiKeyRequest {
  group_id?: number
  concurrency?: number
}

/**
 * Update an API key's group binding
 * @param id - API Key ID
 * @param groupId - Group ID (0 to unbind, positive to bind, null/undefined to skip)
 * @returns Updated API key with auto-grant info
 */
export async function updateApiKey(id: number, updates: AdminUpdateApiKeyRequest): Promise<UpdateApiKeyResult> {
  const { data } = await apiClient.put<UpdateApiKeyResult>(`/admin/api-keys/${id}`, updates)
  return data
}

export async function updateApiKeyGroup(id: number, groupId: number | null): Promise<UpdateApiKeyResult> {
  return updateApiKey(id, { group_id: groupId === null ? 0 : groupId })
}

export const apiKeysAPI = {
  updateApiKey,
  updateApiKeyGroup
}

export default apiKeysAPI
