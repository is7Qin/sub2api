import { apiClient } from './client'
import type { PaginatedResponse } from '@/types'
import type { LotteryCampaign, LotteryChance, LotteryDraw, LotteryDrawResult } from './admin/lottery'

export interface ListLotteryParams {
  page?: number
  page_size?: number
  campaign_id?: number
}

export async function listCampaigns(params: ListLotteryParams = {}): Promise<PaginatedResponse<LotteryCampaign>> {
  const { data } = await apiClient.get<PaginatedResponse<LotteryCampaign>>('/lottery/campaigns', {
    params: {
      page: params.page ?? 1,
      page_size: params.page_size ?? 20,
    },
  })
  return data
}

export async function getCampaign(id: number): Promise<LotteryCampaign> {
  const { data } = await apiClient.get<LotteryCampaign>(`/lottery/campaigns/${id}`)
  return data
}

export async function listChances(params: ListLotteryParams = {}): Promise<PaginatedResponse<LotteryChance>> {
  const { data } = await apiClient.get<PaginatedResponse<LotteryChance>>('/lottery/chances', {
    params: {
      page: params.page ?? 1,
      page_size: params.page_size ?? 20,
      campaign_id: params.campaign_id || undefined,
    },
  })
  return data
}

export async function listDraws(params: ListLotteryParams = {}): Promise<PaginatedResponse<LotteryDraw>> {
  const { data } = await apiClient.get<PaginatedResponse<LotteryDraw>>('/lottery/draws', {
    params: {
      page: params.page ?? 1,
      page_size: params.page_size ?? 20,
      campaign_id: params.campaign_id || undefined,
    },
  })
  return data
}

export async function draw(campaignId: number): Promise<LotteryDrawResult> {
  const { data } = await apiClient.post<LotteryDrawResult>('/lottery/draw', { campaign_id: campaignId })
  return data
}

export const lotteryAPI = {
  listCampaigns,
  getCampaign,
  listChances,
  listDraws,
  draw,
}

export default lotteryAPI
