import { apiClient } from '../client'
import type { BasePaginationResponse } from '@/types'

export type RechargeResetCampaignStatus = 'draft' | 'active' | 'disabled' | 'ended'
export type RechargeResetRuleStatus = 'active' | 'disabled'

export interface RechargeResetCampaignRule {
  id: number
  campaign_id: number
  group_id: number
  threshold_amount: number
  status: RechargeResetRuleStatus
  metadata?: Record<string, unknown> | null
  created_at: string
  updated_at: string
}

export interface RechargeResetCampaign {
  id: number
  name: string
  description: string
  status: RechargeResetCampaignStatus
  starts_at: string
  ends_at?: string | null
  reset_daily: boolean
  reset_weekly: boolean
  reset_monthly: boolean
  metadata?: Record<string, unknown> | null
  created_at: string
  updated_at: string
  rules?: RechargeResetCampaignRule[]
}

export interface RechargeResetRecord {
  id: number
  campaign_id: number
  rule_id: number
  redeem_code_id: number
  user_id: number
  subscription_id: number
  group_id: number
  recharge_amount: number
  threshold_amount: number
  reset_daily: boolean
  reset_weekly: boolean
  reset_monthly: boolean
  metadata?: Record<string, unknown> | null
  created_at: string
}

export interface ListRechargeResetCampaignsParams {
  page?: number
  page_size?: number
  status?: string
}

export interface CreateRechargeResetCampaignRequest {
  name: string
  description?: string
  status?: RechargeResetCampaignStatus
  starts_at: string
  ends_at?: string | null
  reset_daily?: boolean
  reset_weekly?: boolean
  reset_monthly?: boolean
  metadata?: Record<string, unknown>
}

export interface UpdateRechargeResetCampaignRequest {
  name?: string
  description?: string
  status?: RechargeResetCampaignStatus
  starts_at?: string
  ends_at?: string | null
  reset_daily?: boolean
  reset_weekly?: boolean
  reset_monthly?: boolean
  metadata?: Record<string, unknown>
}

export interface CreateRechargeResetRuleRequest {
  group_id: number
  threshold_amount: number
  status?: RechargeResetRuleStatus
  metadata?: Record<string, unknown>
}

export interface UpdateRechargeResetRuleRequest {
  group_id?: number
  threshold_amount?: number
  status?: RechargeResetRuleStatus
  metadata?: Record<string, unknown>
}

function idempotencyConfig(): { headers: { 'Idempotency-Key': string } } {
  const key = typeof globalThis.crypto?.randomUUID === 'function'
    ? globalThis.crypto.randomUUID()
    : `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`
  return { headers: { 'Idempotency-Key': key } }
}

export async function listCampaigns(params: ListRechargeResetCampaignsParams = {}): Promise<BasePaginationResponse<RechargeResetCampaign>> {
  const { data } = await apiClient.get<BasePaginationResponse<RechargeResetCampaign>>('/admin/recharge-reset/campaigns', { params })
  return data
}

export async function getCampaign(id: number): Promise<RechargeResetCampaign> {
  const { data } = await apiClient.get<RechargeResetCampaign>(`/admin/recharge-reset/campaigns/${id}`)
  return data
}

export async function createCampaign(payload: CreateRechargeResetCampaignRequest): Promise<RechargeResetCampaign> {
  const { data } = await apiClient.post<RechargeResetCampaign>('/admin/recharge-reset/campaigns', payload, idempotencyConfig())
  return data
}

export async function updateCampaign(id: number, payload: UpdateRechargeResetCampaignRequest): Promise<RechargeResetCampaign> {
  const { data } = await apiClient.put<RechargeResetCampaign>(`/admin/recharge-reset/campaigns/${id}`, payload, idempotencyConfig())
  return data
}

export async function listRules(campaignId: number): Promise<RechargeResetCampaignRule[]> {
  const { data } = await apiClient.get<RechargeResetCampaignRule[]>(`/admin/recharge-reset/campaigns/${campaignId}/rules`)
  return data
}

export async function createRule(campaignId: number, payload: CreateRechargeResetRuleRequest): Promise<RechargeResetCampaignRule> {
  const { data } = await apiClient.post<RechargeResetCampaignRule>(`/admin/recharge-reset/campaigns/${campaignId}/rules`, payload, idempotencyConfig())
  return data
}

export async function updateRule(ruleId: number, payload: UpdateRechargeResetRuleRequest): Promise<RechargeResetCampaignRule> {
  const { data } = await apiClient.put<RechargeResetCampaignRule>(`/admin/recharge-reset/rules/${ruleId}`, payload, idempotencyConfig())
  return data
}

const rechargeResetAPI = {
  listCampaigns,
  getCampaign,
  createCampaign,
  updateCampaign,
  listRules,
  createRule,
  updateRule,
}

export default rechargeResetAPI
