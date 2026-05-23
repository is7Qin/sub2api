import { apiClient } from '../client'
import type { PaginatedResponse } from '@/types'

export type LotteryCampaignStatus = 'draft' | 'active' | 'disabled' | 'ended'
export type LotteryPrizeStatus = 'active' | 'disabled'
export type LotteryRedeemType = 'balance' | 'concurrency' | 'subscription' | 'invitation' | 'timed_quota' | 'random_timed_quota'

export interface LotteryCampaign {
  id: number
  name: string
  description: string
  status: LotteryCampaignStatus
  starts_at: string
  ends_at?: string | null
  chance_expires_in_days: number
  metadata?: Record<string, unknown> | null
  created_at: string
  updated_at: string
  prizes?: LotteryPrize[]
}

export interface LotteryPrize {
  id: number
  campaign_id: number
  name: string
  description: string
  status: LotteryPrizeStatus
  weight: number
  stock_total: number
  stock_used: number
  redeem_type: LotteryRedeemType
  redeem_value: number
  redeem_group_id?: number | null
  redeem_validity_days: number
  redeem_metadata?: Record<string, unknown> | null
  sort_order: number
  metadata?: Record<string, unknown> | null
  created_at: string
  updated_at: string
}

export interface LotteryChance {
  id: number
  campaign_id: number
  user_id: number
  source: string
  source_id: string
  status: 'available' | 'used' | 'expired'
  expires_at: string
  used_at?: string | null
  metadata?: Record<string, unknown> | null
  created_at: string
  updated_at: string
}

export interface LotteryDraw {
  id: number
  campaign_id: number
  user_id: number
  chance_id: number
  prize_id?: number | null
  redeem_code_id?: number | null
  redeem_code: string
  status: 'pending' | 'awarded' | 'failed'
  error_message?: string
  metadata?: Record<string, unknown> | null
  drawn_at: string
  created_at: string
  updated_at: string
  prize?: LotteryPrize | null
}

export interface LotteryDrawResult {
  draw: LotteryDraw
  prize: LotteryPrize
  redeem_code?: { code?: string; value?: number; type?: string } | null
}

export interface ListLotteryCampaignsParams {
  page?: number
  page_size?: number
  status?: string
}

export interface CreateLotteryCampaignRequest {
  name: string
  description?: string
  status?: LotteryCampaignStatus
  starts_at?: string | null
  ends_at?: string | null
  chance_expires_in_days?: number
  metadata?: Record<string, unknown>
}

export interface UpdateLotteryCampaignRequest {
  name?: string
  description?: string
  status?: LotteryCampaignStatus
  starts_at?: string | null
  ends_at?: string | null
  clear_ends_at?: boolean
  chance_expires_in_days?: number
  metadata?: Record<string, unknown>
}

export interface CreateLotteryPrizeRequest {
  name: string
  description?: string
  status?: LotteryPrizeStatus
  weight: number
  stock_total: number
  redeem_type: LotteryRedeemType
  redeem_value: number
  redeem_group_id?: number | null
  redeem_validity_days?: number
  redeem_metadata?: Record<string, unknown>
  sort_order?: number
  metadata?: Record<string, unknown>
}

export interface UpdateLotteryPrizeRequest {
  name?: string
  description?: string
  status?: LotteryPrizeStatus
  weight?: number
  stock_total?: number
  redeem_type?: LotteryRedeemType
  redeem_value?: number
  redeem_group_id?: number | null
  clear_redeem_group_id?: boolean
  redeem_validity_days?: number
  redeem_metadata?: Record<string, unknown>
  sort_order?: number
  metadata?: Record<string, unknown>
}

export interface GrantLotteryChancesRequest {
  user_id: number
  count?: number
  source?: string
  source_id?: string
  expires_at?: string | null
  metadata?: Record<string, unknown>
}

export async function listCampaigns(params: ListLotteryCampaignsParams = {}): Promise<PaginatedResponse<LotteryCampaign>> {
  const { data } = await apiClient.get<PaginatedResponse<LotteryCampaign>>('/admin/lottery/campaigns', {
    params: {
      page: params.page ?? 1,
      page_size: params.page_size ?? 20,
      status: params.status || undefined,
    },
  })
  return data
}

export async function getCampaign(id: number): Promise<LotteryCampaign> {
  const { data } = await apiClient.get<LotteryCampaign>(`/admin/lottery/campaigns/${id}`)
  return data
}

export async function createCampaign(payload: CreateLotteryCampaignRequest): Promise<LotteryCampaign> {
  const { data } = await apiClient.post<LotteryCampaign>('/admin/lottery/campaigns', payload)
  return data
}

export async function updateCampaign(id: number, payload: UpdateLotteryCampaignRequest): Promise<LotteryCampaign> {
  const { data } = await apiClient.put<LotteryCampaign>(`/admin/lottery/campaigns/${id}`, payload)
  return data
}

export async function listPrizes(campaignId: number): Promise<LotteryPrize[]> {
  const { data } = await apiClient.get<LotteryPrize[]>(`/admin/lottery/campaigns/${campaignId}/prizes`)
  return data
}

export async function createPrize(campaignId: number, payload: CreateLotteryPrizeRequest): Promise<LotteryPrize> {
  const { data } = await apiClient.post<LotteryPrize>(`/admin/lottery/campaigns/${campaignId}/prizes`, payload)
  return data
}

export async function updatePrize(prizeId: number, payload: UpdateLotteryPrizeRequest): Promise<LotteryPrize> {
  const { data } = await apiClient.put<LotteryPrize>(`/admin/lottery/prizes/${prizeId}`, payload)
  return data
}

export async function grantChances(campaignId: number, payload: GrantLotteryChancesRequest): Promise<LotteryChance[]> {
  const { data } = await apiClient.post<LotteryChance[]>(`/admin/lottery/campaigns/${campaignId}/chances`, payload)
  return data
}

export const lotteryAPI = {
  listCampaigns,
  getCampaign,
  createCampaign,
  updateCampaign,
  listPrizes,
  createPrize,
  updatePrize,
  grantChances,
}

export default lotteryAPI
