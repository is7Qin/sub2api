import { apiClient } from '../client'
import type { AxiosRequestConfig } from 'axios'
import type { BasePaginationResponse } from '@/types'

export type RankingRewardCampaignStatus = 'draft' | 'active' | 'disabled' | 'ended'
export type RankingRewardRunStatus = 'running' | 'completed' | 'failed'

export interface RankingRewardCampaign {
  id: number
  name: string
  description: string
  status: RankingRewardCampaignStatus
  lottery_campaign_id: number
  top_n: number
  chance_count: number
  public_display_limit: number
  min_actual_cost: number
  starts_at: string
  ends_at?: string | null
  timezone: string
  last_run_date?: string | null
  metadata?: Record<string, unknown> | null
  created_at: string
  updated_at: string
}

export interface RankingRewardExclusion {
  id: number
  campaign_id: number
  user_id: number
  reason: string
  created_at: string
  updated_at: string
}

export interface RankingRewardRun {
  id: number
  campaign_id: number
  reward_date: string
  window_start: string
  window_end: string
  status: RankingRewardRunStatus
  awarded_count: number
  total_actual_cost: number
  error_message?: string
  metadata?: Record<string, unknown> | null
  started_at: string
  finished_at?: string | null
  created_at: string
  updated_at: string
}

export interface RankingRewardAward {
  id: number
  run_id: number
  campaign_id: number
  lottery_campaign_id: number
  user_id: number
  rank: number
  actual_cost: number
  requests: number
  tokens: number
  chance_count: number
  lottery_chance_ids: number[]
  metadata?: Record<string, unknown> | null
  created_at: string
}

export interface RankingRewardCandidate {
  user_id: number
  email: string
  rank: number
  actual_cost: number
  requests: number
  tokens: number
}

export interface RankingRewardRunResult {
  run: RankingRewardRun
  awards: RankingRewardAward[]
  ranks?: RankingRewardCandidate[]
}

export interface ListRankingRewardCampaignsParams {
  page?: number
  page_size?: number
  status?: string
}

export interface ListRankingRewardParams {
  page?: number
  page_size?: number
}

export interface CreateRankingRewardCampaignRequest {
  name: string
  description?: string
  status?: RankingRewardCampaignStatus
  lottery_campaign_id: number
  top_n?: number
  chance_count?: number
  public_display_limit?: number
  min_actual_cost?: number
  starts_at?: string
  ends_at?: string | null
  timezone?: string
  metadata?: Record<string, unknown>
}

export interface UpdateRankingRewardCampaignRequest {
  name?: string
  description?: string
  status?: RankingRewardCampaignStatus
  lottery_campaign_id?: number
  top_n?: number
  chance_count?: number
  public_display_limit?: number
  min_actual_cost?: number
  starts_at?: string
  ends_at?: string | null
  clear_ends_at?: boolean
  timezone?: string
  last_run_date?: string | null
  clear_last_run_date?: boolean
  metadata?: Record<string, unknown>
}

export interface CreateRankingRewardExclusionRequest {
  user_id: number
  reason?: string
}

export interface UpdateRankingRewardExclusionRequest {
  reason?: string
}

export interface RunRankingRewardCampaignRequest {
  reward_date?: string
}

export interface RankingRewardRequestOptions {
  idempotencyKey?: string
}

function withIdempotencyKey(options?: RankingRewardRequestOptions): AxiosRequestConfig | undefined {
  return options?.idempotencyKey
    ? { headers: { 'Idempotency-Key': options.idempotencyKey } }
    : undefined
}

export async function listCampaigns(params: ListRankingRewardCampaignsParams = {}): Promise<BasePaginationResponse<RankingRewardCampaign>> {
  const { data } = await apiClient.get<BasePaginationResponse<RankingRewardCampaign>>('/admin/ranking-rewards/campaigns', { params })
  return data
}

export async function getCampaign(id: number): Promise<RankingRewardCampaign> {
  const { data } = await apiClient.get<RankingRewardCampaign>(`/admin/ranking-rewards/campaigns/${id}`)
  return data
}

export async function createCampaign(payload: CreateRankingRewardCampaignRequest, options?: RankingRewardRequestOptions): Promise<RankingRewardCampaign> {
  const { data } = await apiClient.post<RankingRewardCampaign>('/admin/ranking-rewards/campaigns', payload, withIdempotencyKey(options))
  return data
}

export async function updateCampaign(id: number, payload: UpdateRankingRewardCampaignRequest): Promise<RankingRewardCampaign> {
  const { data } = await apiClient.put<RankingRewardCampaign>(`/admin/ranking-rewards/campaigns/${id}`, payload)
  return data
}

export async function runCampaign(id: number, payload: RunRankingRewardCampaignRequest, options?: RankingRewardRequestOptions): Promise<RankingRewardRunResult> {
  const { data } = await apiClient.post<RankingRewardRunResult>(`/admin/ranking-rewards/campaigns/${id}/run`, payload, withIdempotencyKey(options))
  return data
}

export async function listExclusions(campaignId: number, params: ListRankingRewardParams = {}): Promise<BasePaginationResponse<RankingRewardExclusion>> {
  const { data } = await apiClient.get<BasePaginationResponse<RankingRewardExclusion>>(`/admin/ranking-rewards/campaigns/${campaignId}/exclusions`, { params })
  return data
}

export async function createExclusion(campaignId: number, payload: CreateRankingRewardExclusionRequest, options?: RankingRewardRequestOptions): Promise<RankingRewardExclusion> {
  const { data } = await apiClient.post<RankingRewardExclusion>(`/admin/ranking-rewards/campaigns/${campaignId}/exclusions`, payload, withIdempotencyKey(options))
  return data
}

export async function updateExclusion(id: number, payload: UpdateRankingRewardExclusionRequest): Promise<RankingRewardExclusion> {
  const { data } = await apiClient.put<RankingRewardExclusion>(`/admin/ranking-rewards/exclusions/${id}`, payload)
  return data
}

export async function deleteExclusion(id: number): Promise<{ deleted: boolean }> {
  const { data } = await apiClient.delete<{ deleted: boolean }>(`/admin/ranking-rewards/exclusions/${id}`)
  return data
}

export async function listRuns(campaignId: number, params: ListRankingRewardParams = {}): Promise<BasePaginationResponse<RankingRewardRun>> {
  const { data } = await apiClient.get<BasePaginationResponse<RankingRewardRun>>(`/admin/ranking-rewards/campaigns/${campaignId}/runs`, { params })
  return data
}

export async function listAwards(runId: number, params: ListRankingRewardParams = {}): Promise<BasePaginationResponse<RankingRewardAward>> {
  const { data } = await apiClient.get<BasePaginationResponse<RankingRewardAward>>(`/admin/ranking-rewards/runs/${runId}/awards`, { params })
  return data
}

const rankingRewardAPI = {
  listCampaigns,
  getCampaign,
  createCampaign,
  updateCampaign,
  runCampaign,
  listExclusions,
  createExclusion,
  updateExclusion,
  deleteExclusion,
  listRuns,
  listAwards,
}

export default rankingRewardAPI
