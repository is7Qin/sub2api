import { apiClient } from './client'

export interface PublicRankingRewardEntry {
  rank: number
  display_name: string
  is_current_user: boolean
  awarded: boolean
  chance_count: number
}

export interface PublicRankingRewardLeaderboard {
  board_key: string
  campaign_name: string
  reward_date: string
  status: 'live' | 'completed'
  awarded_count: number
  public_display_limit: number
  entries: PublicRankingRewardEntry[]
}

export async function getLeaderboards(runLimit = 10): Promise<PublicRankingRewardLeaderboard[]> {
  const { data } = await apiClient.get<PublicRankingRewardLeaderboard[]>('/ranking-rewards/leaderboards', {
    params: { run_limit: runLimit }
  })
  return data || []
}

export const rankingRewardAPI = {
  getLeaderboards
}

export default rankingRewardAPI
