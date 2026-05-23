<template>
  <AppLayout>
    <div class="space-y-6">
      <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div>
          <h1 class="text-2xl font-bold text-gray-900 dark:text-white">{{ t('admin.rankingReward.title') }}</h1>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.rankingReward.description') }}</p>
        </div>
        <button class="btn btn-secondary" :disabled="loadingCampaigns" @click="loadCampaigns()">
          <Icon name="refresh" size="sm" :class="loadingCampaigns ? 'animate-spin' : ''" />
          <span>{{ t('common.refresh') }}</span>
        </button>
      </div>

      <div class="grid gap-4 sm:grid-cols-4">
        <div class="card p-5">
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ tx('活动数', 'Campaigns') }}</p>
          <p class="mt-2 text-2xl font-semibold text-gray-900 dark:text-white">{{ campaigns.length }}</p>
        </div>
        <div class="card p-5">
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ tx('启用中', 'Active') }}</p>
          <p class="mt-2 text-2xl font-semibold text-emerald-600 dark:text-emerald-400">{{ activeCampaignCount }}</p>
        </div>
        <div class="card p-5">
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ tx('最近发放', 'Last Awards') }}</p>
          <p class="mt-2 text-2xl font-semibold text-primary-600 dark:text-primary-400">{{ selectedRun?.awarded_count ?? '-' }}</p>
        </div>
        <div class="card p-5">
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ tx('发放消耗', 'Awarded Cost') }}</p>
          <p class="mt-2 text-2xl font-semibold text-amber-600 dark:text-amber-400">{{ selectedRun ? formatMoney(selectedRun.total_actual_cost) : '-' }}</p>
        </div>
      </div>

      <div class="grid gap-6 xl:grid-cols-[minmax(0,1.15fr)_minmax(380px,0.85fr)]">
        <div class="card overflow-hidden">
          <div class="flex flex-col gap-3 border-b border-gray-100 px-6 py-4 dark:border-dark-700 sm:flex-row sm:items-center sm:justify-between">
            <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ tx('排行榜奖励活动', 'Ranking Reward Campaigns') }}</h2>
            <div class="flex gap-2">
              <select v-model="statusFilter" class="input w-36" @change="loadCampaigns()">
                <option value="">{{ tx('全部状态', 'All Status') }}</option>
                <option value="draft">{{ statusLabel('draft') }}</option>
                <option value="active">{{ statusLabel('active') }}</option>
                <option value="disabled">{{ statusLabel('disabled') }}</option>
                <option value="ended">{{ statusLabel('ended') }}</option>
              </select>
              <button class="btn btn-primary" @click="startCreateCampaign">
                <Icon name="plus" size="sm" />
                <span>{{ tx('新建活动', 'New Campaign') }}</span>
              </button>
            </div>
          </div>

          <div v-if="loadingCampaigns" class="flex justify-center py-12">
            <div class="h-8 w-8 animate-spin rounded-full border-2 border-primary-500 border-t-transparent"></div>
          </div>
          <div v-else-if="campaigns.length === 0" class="p-8 text-center text-sm text-gray-500 dark:text-gray-400">
            {{ tx('暂无排行榜奖励活动', 'No ranking reward campaigns yet.') }}
          </div>
          <div v-else class="overflow-x-auto">
            <table class="w-full min-w-[900px] text-left text-sm">
              <thead>
                <tr class="border-b border-gray-100 text-gray-500 dark:border-dark-700 dark:text-gray-400">
                  <th class="px-6 py-3 font-medium">{{ tx('名称', 'Name') }}</th>
                  <th class="px-4 py-3 font-medium">{{ tx('状态', 'Status') }}</th>
                  <th class="px-4 py-3 font-medium">{{ tx('抽奖活动ID', 'Lottery Campaign') }}</th>
                  <th class="px-4 py-3 font-medium">{{ tx('名额/展示/机会', 'Top / Display / Chances') }}</th>
                  <th class="px-4 py-3 font-medium">{{ tx('最低消耗', 'Min Cost') }}</th>
                  <th class="px-4 py-3 font-medium">{{ tx('上次运行', 'Last Run') }}</th>
                  <th class="px-6 py-3 text-right font-medium">{{ tx('操作', 'Actions') }}</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="campaign in campaigns" :key="campaign.id" class="border-b border-gray-100 last:border-b-0 dark:border-dark-800">
                  <td class="px-6 py-4">
                    <button class="text-left font-medium text-primary-600 hover:underline dark:text-primary-400" @click="selectCampaign(campaign)">
                      {{ campaign.name }}
                    </button>
                    <p class="mt-1 max-w-xs truncate text-xs text-gray-500 dark:text-gray-400">{{ campaign.description || '-' }}</p>
                  </td>
                  <td class="px-4 py-4">
                    <span class="rounded-full px-2 py-0.5 text-xs font-medium" :class="campaignStatusClass(campaign.status)">
                      {{ statusLabel(campaign.status) }}
                    </span>
                  </td>
                  <td class="px-4 py-4 text-gray-700 dark:text-gray-300">#{{ campaign.lottery_campaign_id }}</td>
                  <td class="px-4 py-4 text-gray-700 dark:text-gray-300">{{ campaign.top_n }} / {{ campaign.public_display_limit }} / {{ campaign.chance_count }}</td>
                  <td class="px-4 py-4 text-gray-700 dark:text-gray-300">{{ formatMoney(campaign.min_actual_cost) }}</td>
                  <td class="px-4 py-4 text-gray-700 dark:text-gray-300">{{ formatDateOnly(campaign.last_run_date) || '-' }}</td>
                  <td class="px-6 py-4">
                    <div class="flex justify-end gap-2">
                      <button class="btn btn-sm btn-secondary" @click="startEditCampaign(campaign)">{{ tx('编辑', 'Edit') }}</button>
                      <button class="btn btn-sm btn-primary" :disabled="runningCampaignId === campaign.id" @click="confirmRun(campaign)">
                        <Icon v-if="runningCampaignId === campaign.id" name="refresh" size="xs" class="animate-spin" />
                        <span>{{ tx('执行', 'Run') }}</span>
                      </button>
                    </div>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>

        <div class="card">
          <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
            <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ formMode === 'create' ? tx('新建奖励活动', 'Create Campaign') : tx('编辑奖励活动', 'Edit Campaign') }}</h2>
          </div>
          <form class="space-y-4 p-6" @submit.prevent="saveCampaign">
            <div>
              <label class="label">{{ tx('名称', 'Name') }}</label>
              <input v-model="campaignForm.name" class="input" required />
            </div>
            <div>
              <label class="label">{{ tx('说明', 'Description') }}</label>
              <textarea v-model="campaignForm.description" class="input min-h-[80px]"></textarea>
            </div>
            <div class="grid gap-4 sm:grid-cols-2">
              <div>
                <label class="label">{{ tx('状态', 'Status') }}</label>
                <select v-model="campaignForm.status" class="input">
                  <option value="draft">{{ statusLabel('draft') }}</option>
                  <option value="active">{{ statusLabel('active') }}</option>
                  <option value="disabled">{{ statusLabel('disabled') }}</option>
                  <option value="ended">{{ statusLabel('ended') }}</option>
                </select>
              </div>
              <div>
                <label class="label">{{ tx('抽奖活动ID', 'Lottery Campaign ID') }}</label>
                <input v-model.number="campaignForm.lottery_campaign_id" class="input" min="1" required type="number" />
              </div>
            </div>
            <div class="grid gap-4 sm:grid-cols-2">
              <div>
                <label class="label">{{ tx('前 N 名', 'Top N') }}</label>
                <input v-model.number="campaignForm.top_n" class="input" min="1" max="1000" type="number" />
              </div>
              <div>
                <label class="label">{{ tx('用户榜单显示前 N 名', 'Public Display Top N') }}</label>
                <input v-model.number="campaignForm.public_display_limit" class="input" min="1" max="1000" type="number" />
                <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ tx('仅影响用户可见榜单，不影响实际获奖人数', 'Only affects the user-facing leaderboard, not winners.') }}</p>
              </div>
              <div>
                <label class="label">{{ tx('每人机会数', 'Chances/User') }}</label>
                <input v-model.number="campaignForm.chance_count" class="input" min="1" max="1000" type="number" />
              </div>
              <div>
                <label class="label">{{ tx('最低实际消耗', 'Min Actual Cost') }}</label>
                <input v-model.number="campaignForm.min_actual_cost" class="input" min="0" step="0.01" type="number" />
              </div>
            </div>
            <div class="grid gap-4 sm:grid-cols-2">
              <div>
                <label class="label">{{ tx('开始时间', 'Starts At') }}</label>
                <input v-model="campaignForm.starts_at" class="input" type="datetime-local" />
              </div>
              <div>
                <label class="label">{{ tx('结束时间', 'Ends At') }}</label>
                <input v-model="campaignForm.ends_at" class="input" type="datetime-local" />
              </div>
            </div>
            <div>
              <label class="label">{{ tx('时区', 'Timezone') }}</label>
              <input v-model="campaignForm.timezone" class="input" placeholder="Asia/Shanghai" />
            </div>
            <div class="flex justify-end gap-2">
              <button v-if="formMode === 'edit'" class="btn btn-secondary" type="button" @click="startCreateCampaign">{{ tx('取消编辑', 'Cancel Edit') }}</button>
              <button class="btn btn-primary" :disabled="savingCampaign" type="submit">
                <Icon v-if="savingCampaign" name="refresh" size="sm" class="animate-spin" />
                <span>{{ tx('保存', 'Save') }}</span>
              </button>
            </div>
          </form>
        </div>
      </div>

      <div v-if="selectedCampaign" class="grid gap-6 xl:grid-cols-2">
        <div class="card overflow-hidden">
          <div class="flex items-center justify-between border-b border-gray-100 px-6 py-4 dark:border-dark-700">
            <div>
              <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ tx('排除用户', 'Excluded Users') }}</h2>
              <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ selectedCampaign.name }}</p>
            </div>
          </div>
          <form class="grid gap-3 border-b border-gray-100 p-6 dark:border-dark-700 sm:grid-cols-[120px_minmax(0,1fr)_auto]" @submit.prevent="addExclusion">
            <input v-model.number="exclusionForm.user_id" class="input" min="1" :placeholder="tx('用户ID', 'User ID')" required type="number" />
            <input v-model="exclusionForm.reason" class="input" :placeholder="tx('原因', 'Reason')" />
            <button class="btn btn-primary" :disabled="savingExclusion" type="submit">{{ tx('添加', 'Add') }}</button>
          </form>
          <div v-if="exclusions.length === 0" class="p-8 text-center text-sm text-gray-500 dark:text-gray-400">
            {{ tx('暂无排除用户', 'No excluded users.') }}
          </div>
          <div v-else class="overflow-x-auto">
            <table class="w-full min-w-[520px] text-left text-sm">
              <thead>
                <tr class="border-b border-gray-100 text-gray-500 dark:border-dark-700 dark:text-gray-400">
                  <th class="px-6 py-3 font-medium">{{ tx('用户ID', 'User ID') }}</th>
                  <th class="px-4 py-3 font-medium">{{ tx('原因', 'Reason') }}</th>
                  <th class="px-4 py-3 font-medium">{{ tx('创建时间', 'Created') }}</th>
                  <th class="px-6 py-3 text-right font-medium">{{ tx('操作', 'Actions') }}</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="item in exclusions" :key="item.id" class="border-b border-gray-100 last:border-b-0 dark:border-dark-800">
                  <td class="px-6 py-4 text-gray-900 dark:text-white">#{{ item.user_id }}</td>
                  <td class="px-4 py-4 text-gray-700 dark:text-gray-300">{{ item.reason || '-' }}</td>
                  <td class="px-4 py-4 text-gray-700 dark:text-gray-300">{{ formatDateTime(item.created_at) }}</td>
                  <td class="px-6 py-4 text-right">
                    <button class="text-sm text-red-600 hover:underline dark:text-red-400" @click="removeExclusion(item.id)">{{ tx('删除', 'Delete') }}</button>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>

        <div class="card overflow-hidden">
          <div class="flex flex-col gap-3 border-b border-gray-100 px-6 py-4 dark:border-dark-700 sm:flex-row sm:items-center sm:justify-between">
            <div>
              <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ tx('运行记录', 'Runs') }}</h2>
              <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ tx('手动执行默认会发放上一自然日排行榜奖励', 'Manual run defaults to the previous local day.') }}</p>
            </div>
            <form class="flex gap-2" @submit.prevent="confirmRun(selectedCampaign, runDate)">
              <input v-model="runDate" class="input w-40" type="date" />
              <button class="btn btn-primary" :disabled="runningCampaignId === selectedCampaign.id" type="submit">{{ tx('执行', 'Run') }}</button>
            </form>
          </div>
          <div v-if="runs.length === 0" class="p-8 text-center text-sm text-gray-500 dark:text-gray-400">
            {{ tx('暂无运行记录', 'No runs yet.') }}
          </div>
          <div v-else class="overflow-x-auto">
            <table class="w-full min-w-[720px] text-left text-sm">
              <thead>
                <tr class="border-b border-gray-100 text-gray-500 dark:border-dark-700 dark:text-gray-400">
                  <th class="px-6 py-3 font-medium">{{ tx('奖励日期', 'Reward Date') }}</th>
                  <th class="px-4 py-3 font-medium">{{ tx('状态', 'Status') }}</th>
                  <th class="px-4 py-3 font-medium">{{ tx('发放人数', 'Awards') }}</th>
                  <th class="px-4 py-3 font-medium">{{ tx('总消耗', 'Cost') }}</th>
                  <th class="px-4 py-3 font-medium">{{ tx('完成时间', 'Finished') }}</th>
                  <th class="px-6 py-3 text-right font-medium">{{ tx('操作', 'Actions') }}</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="run in runs" :key="run.id" class="border-b border-gray-100 last:border-b-0 dark:border-dark-800">
                  <td class="px-6 py-4 text-gray-900 dark:text-white">{{ formatDateOnly(run.reward_date) }}</td>
                  <td class="px-4 py-4">
                    <span class="rounded-full px-2 py-0.5 text-xs font-medium" :class="runStatusClass(run.status)">{{ runStatusLabel(run.status) }}</span>
                  </td>
                  <td class="px-4 py-4 text-gray-700 dark:text-gray-300">{{ run.awarded_count }}</td>
                  <td class="px-4 py-4 text-gray-700 dark:text-gray-300">{{ formatMoney(run.total_actual_cost) }}</td>
                  <td class="px-4 py-4 text-gray-700 dark:text-gray-300">{{ formatDateTime(run.finished_at) || '-' }}</td>
                  <td class="px-6 py-4 text-right">
                    <button class="text-sm text-primary-600 hover:underline dark:text-primary-400" @click="selectRun(run)">{{ tx('查看奖励', 'Awards') }}</button>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </div>

      <div v-if="selectedRun" class="card overflow-hidden">
        <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
          <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ tx('发放明细', 'Award Details') }}</h2>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ tx('运行ID', 'Run ID') }} #{{ selectedRun.id }}</p>
        </div>
        <div v-if="awards.length === 0" class="p-8 text-center text-sm text-gray-500 dark:text-gray-400">
          {{ tx('暂无发放明细', 'No awards found.') }}
        </div>
        <div v-else class="overflow-x-auto">
          <table class="w-full min-w-[900px] text-left text-sm">
            <thead>
              <tr class="border-b border-gray-100 text-gray-500 dark:border-dark-700 dark:text-gray-400">
                <th class="px-6 py-3 font-medium">{{ tx('名次', 'Rank') }}</th>
                <th class="px-4 py-3 font-medium">{{ tx('用户ID', 'User ID') }}</th>
                <th class="px-4 py-3 font-medium">{{ tx('实际消耗', 'Actual Cost') }}</th>
                <th class="px-4 py-3 font-medium">{{ tx('请求数', 'Requests') }}</th>
                <th class="px-4 py-3 font-medium">{{ tx('Token', 'Tokens') }}</th>
                <th class="px-4 py-3 font-medium">{{ tx('机会数', 'Chances') }}</th>
                <th class="px-6 py-3 font-medium">{{ tx('抽奖机会ID', 'Lottery Chance IDs') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="award in awards" :key="award.id" class="border-b border-gray-100 last:border-b-0 dark:border-dark-800">
                <td class="px-6 py-4 font-semibold text-gray-900 dark:text-white">#{{ award.rank }}</td>
                <td class="px-4 py-4 text-gray-700 dark:text-gray-300">#{{ award.user_id }}</td>
                <td class="px-4 py-4 text-gray-700 dark:text-gray-300">{{ formatMoney(award.actual_cost) }}</td>
                <td class="px-4 py-4 text-gray-700 dark:text-gray-300">{{ formatNumber(award.requests) }}</td>
                <td class="px-4 py-4 text-gray-700 dark:text-gray-300">{{ formatNumber(award.tokens) }}</td>
                <td class="px-4 py-4 text-gray-700 dark:text-gray-300">{{ award.chance_count }}</td>
                <td class="px-6 py-4 font-mono text-xs text-gray-700 dark:text-gray-300">{{ award.lottery_chance_ids?.join(', ') || '-' }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import Icon from '@/components/icons/Icon.vue'
import rankingRewardAPI from '@/api/admin/rankingReward'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatCurrency, formatDateOnly, formatDateTime, formatNumber, formatDateTimeLocalInput } from '@/utils/format'
import type {
  CreateRankingRewardCampaignRequest,
  RankingRewardAward,
  RankingRewardCampaign,
  RankingRewardCampaignStatus,
  RankingRewardExclusion,
  RankingRewardRun,
  RankingRewardRunStatus,
} from '@/api/admin/rankingReward'

const MAX_RANKING_REWARD_TOP_N = 1000
const MAX_RANKING_REWARD_CHANCE_COUNT = 1000
const MAX_RANKING_REWARD_PUBLIC_DISPLAY_LIMIT = 1000
const MAX_TOTAL_CHANCES = 10000
const IDEMPOTENCY_KEY_PREFIX = 'ranking-reward'

const { t, locale } = useI18n()
const appStore = useAppStore()

const loadingCampaigns = ref(false)
const savingCampaign = ref(false)
const savingExclusion = ref(false)
const runningCampaignId = ref<number | null>(null)
const statusFilter = ref('')
const campaigns = ref<RankingRewardCampaign[]>([])
const selectedCampaign = ref<RankingRewardCampaign | null>(null)
const exclusions = ref<RankingRewardExclusion[]>([])
const runs = ref<RankingRewardRun[]>([])
const selectedRun = ref<RankingRewardRun | null>(null)
const awards = ref<RankingRewardAward[]>([])
const formMode = ref<'create' | 'edit'>('create')
const editingCampaignId = ref<number | null>(null)
const runDate = ref('')

const activeCampaignCount = computed(() => campaigns.value.filter((item) => item.status === 'active').length)

const campaignForm = reactive({
  name: '',
  description: '',
  status: 'draft' as RankingRewardCampaignStatus,
  lottery_campaign_id: 0,
  top_n: 10,
  chance_count: 1,
  public_display_limit: 10,
  min_actual_cost: 0,
  starts_at: '',
  ends_at: '',
  timezone: 'Asia/Shanghai',
})

const exclusionForm = reactive({
  user_id: 0,
  reason: '',
})

function tx(zh: string, en: string): string {
  return String(locale.value).startsWith('zh') ? zh : en
}

function formatMoney(value: number | null | undefined): string {
  return formatCurrency(value || 0)
}

function toLocalInput(value: string | null | undefined): string {
  if (!value) return ''
  const timestamp = new Date(value).getTime()
  return Number.isNaN(timestamp) ? '' : formatDateTimeLocalInput(Math.floor(timestamp / 1000))
}

function localInputToISO(value: string): string | undefined {
  if (!value) return undefined
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? undefined : date.toISOString()
}

function getTimeZoneOffsetMs(timeZone: string, date: Date): number {
  const parts = new Intl.DateTimeFormat('en-US', {
    timeZone,
    hourCycle: 'h23',
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  }).formatToParts(date)
  const values = Object.fromEntries(parts.map((part) => [part.type, part.value]))
  const asUTC = Date.UTC(
    Number(values.year),
    Number(values.month) - 1,
    Number(values.day),
    Number(values.hour),
    Number(values.minute),
    Number(values.second),
  )
  return asUTC - date.getTime()
}

function dateInputToISO(value: string, timeZone: string): string | undefined {
  if (!value) return undefined
  const [year, month, day] = value.split('-').map(Number)
  if (!year || !month || !day) return undefined
  const noonUtc = new Date(Date.UTC(year, month - 1, day, 12))
  const offsetMs = getTimeZoneOffsetMs(timeZone || 'Asia/Shanghai', noonUtc)
  return new Date(noonUtc.getTime() - offsetMs).toISOString()
}

function createIdempotencyKey(action: string): string {
  const random = globalThis.crypto?.randomUUID?.() || `${Date.now()}-${Math.random().toString(36).slice(2)}`
  return `${IDEMPOTENCY_KEY_PREFIX}:${action}:${random}`
}

function statusLabel(status: RankingRewardCampaignStatus): string {
  const labels: Record<RankingRewardCampaignStatus, [string, string]> = {
    draft: ['草稿', 'Draft'],
    active: ['启用', 'Active'],
    disabled: ['停用', 'Disabled'],
    ended: ['已结束', 'Ended'],
  }
  const [zh, en] = labels[status] || [status, status]
  return tx(zh, en)
}

function runStatusLabel(status: RankingRewardRunStatus): string {
  const labels: Record<RankingRewardRunStatus, [string, string]> = {
    running: ['运行中', 'Running'],
    completed: ['已完成', 'Completed'],
    failed: ['失败', 'Failed'],
  }
  const [zh, en] = labels[status] || [status, status]
  return tx(zh, en)
}

function campaignStatusClass(status: string): string {
  if (status === 'active') return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300'
  if (status === 'draft') return 'bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-300'
  if (status === 'disabled') return 'bg-gray-100 text-gray-700 dark:bg-dark-700 dark:text-gray-300'
  return 'bg-blue-100 text-blue-700 dark:bg-blue-900/40 dark:text-blue-300'
}

function runStatusClass(status: string): string {
  if (status === 'completed') return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300'
  if (status === 'failed') return 'bg-red-100 text-red-700 dark:bg-red-900/40 dark:text-red-300'
  return 'bg-blue-100 text-blue-700 dark:bg-blue-900/40 dark:text-blue-300'
}

function resetCampaignForm(): void {
  Object.assign(campaignForm, {
    name: '',
    description: '',
    status: 'draft',
    lottery_campaign_id: 0,
    top_n: 10,
    chance_count: 1,
    public_display_limit: 10,
    min_actual_cost: 0,
    starts_at: '',
    ends_at: '',
    timezone: 'Asia/Shanghai',
  })
}

function startCreateCampaign(): void {
  formMode.value = 'create'
  editingCampaignId.value = null
  resetCampaignForm()
}

function startEditCampaign(campaign: RankingRewardCampaign): void {
  formMode.value = 'edit'
  editingCampaignId.value = campaign.id
  Object.assign(campaignForm, {
    name: campaign.name,
    description: campaign.description,
    status: campaign.status,
    lottery_campaign_id: campaign.lottery_campaign_id,
    top_n: campaign.top_n,
    chance_count: campaign.chance_count,
    public_display_limit: campaign.public_display_limit || campaign.top_n,
    min_actual_cost: campaign.min_actual_cost,
    starts_at: toLocalInput(campaign.starts_at),
    ends_at: toLocalInput(campaign.ends_at),
    timezone: campaign.timezone || 'Asia/Shanghai',
  })
}

function buildCampaignPayload(): CreateRankingRewardCampaignRequest | null {
  const topN = Number(campaignForm.top_n)
  const chanceCount = Number(campaignForm.chance_count)
  const publicDisplayLimit = Number(campaignForm.public_display_limit)
  if (!Number.isFinite(topN) || !Number.isFinite(chanceCount) || !Number.isFinite(publicDisplayLimit) || topN <= 0 || chanceCount <= 0 || publicDisplayLimit <= 0) {
    appStore.showError(tx('前 N 名、榜单显示数量和每人机会数必须大于 0', 'Top N, public display count, and chances per user must be greater than 0'))
    return null
  }
  if (topN > MAX_RANKING_REWARD_TOP_N || chanceCount > MAX_RANKING_REWARD_CHANCE_COUNT || publicDisplayLimit > MAX_RANKING_REWARD_PUBLIC_DISPLAY_LIMIT) {
    appStore.showError(tx('前 N 名、榜单显示数量和每人机会数都不能超过 1000', 'Top N, public display count, and chances per user cannot exceed 1,000'))
    return null
  }
  if (publicDisplayLimit > topN) {
    appStore.showError(tx('用户榜单显示数量不能超过前 N 名', 'Public display count cannot exceed Top N'))
    return null
  }
  if (topN * chanceCount > MAX_TOTAL_CHANCES) {
    appStore.showError(tx('总机会数不能超过 10000', 'Total chances cannot exceed 10,000'))
    return null
  }
  const payload: CreateRankingRewardCampaignRequest = {
    name: campaignForm.name.trim(),
    description: campaignForm.description,
    status: campaignForm.status,
    lottery_campaign_id: Number(campaignForm.lottery_campaign_id),
    top_n: topN,
    chance_count: chanceCount,
    public_display_limit: publicDisplayLimit,
    min_actual_cost: Number(campaignForm.min_actual_cost) || 0,
    timezone: campaignForm.timezone.trim() || 'Asia/Shanghai',
  }
  const startsAt = localInputToISO(campaignForm.starts_at)
  const endsAt = localInputToISO(campaignForm.ends_at)
  if (startsAt) payload.starts_at = startsAt
  if (endsAt) payload.ends_at = endsAt
  return payload
}

async function loadCampaigns(): Promise<void> {
  loadingCampaigns.value = true
  try {
    const result = await rankingRewardAPI.listCampaigns({ page: 1, page_size: 100, status: statusFilter.value || undefined })
    campaigns.value = result.items || []
    if (!selectedCampaign.value && campaigns.value.length > 0) {
      await selectCampaign(campaigns.value[0])
    } else if (selectedCampaign.value) {
      const updated = campaigns.value.find((item) => item.id === selectedCampaign.value?.id)
      if (updated) selectedCampaign.value = updated
    }
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tx('加载排行榜奖励失败', 'Failed to load ranking rewards')))
  } finally {
    loadingCampaigns.value = false
  }
}

async function saveCampaign(): Promise<void> {
  if (!campaignForm.name.trim() || !campaignForm.lottery_campaign_id) return
  savingCampaign.value = true
  try {
    const payload = buildCampaignPayload()
    if (!payload) return
    const item = formMode.value === 'edit' && editingCampaignId.value
      ? await rankingRewardAPI.updateCampaign(editingCampaignId.value, {
        ...payload,
        clear_ends_at: !campaignForm.ends_at,
      })
      : await rankingRewardAPI.createCampaign(payload, { idempotencyKey: createIdempotencyKey('create-campaign') })
    appStore.showSuccess(tx('排行榜奖励已保存', 'Ranking reward campaign saved'))
    await loadCampaigns()
    await selectCampaign(item)
    if (formMode.value === 'create') resetCampaignForm()
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tx('保存排行榜奖励失败', 'Failed to save ranking reward campaign')))
  } finally {
    savingCampaign.value = false
  }
}

async function selectCampaign(campaign: RankingRewardCampaign): Promise<void> {
  selectedCampaign.value = campaign
  selectedRun.value = null
  awards.value = []
  await Promise.all([loadExclusions(campaign.id), loadRuns(campaign.id)])
}

async function loadExclusions(campaignId: number): Promise<void> {
  try {
    const result = await rankingRewardAPI.listExclusions(campaignId, { page: 1, page_size: 100 })
    exclusions.value = result.items || []
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tx('加载排除用户失败', 'Failed to load exclusions')))
  }
}

async function addExclusion(): Promise<void> {
  if (!selectedCampaign.value || !exclusionForm.user_id) return
  savingExclusion.value = true
  try {
    await rankingRewardAPI.createExclusion(selectedCampaign.value.id, {
      user_id: Number(exclusionForm.user_id),
      reason: exclusionForm.reason,
    }, { idempotencyKey: createIdempotencyKey('create-exclusion') })
    exclusionForm.user_id = 0
    exclusionForm.reason = ''
    await loadExclusions(selectedCampaign.value.id)
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tx('添加排除用户失败', 'Failed to add exclusion')))
  } finally {
    savingExclusion.value = false
  }
}

async function removeExclusion(id: number): Promise<void> {
  if (!selectedCampaign.value) return
  try {
    await rankingRewardAPI.deleteExclusion(id)
    await loadExclusions(selectedCampaign.value.id)
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tx('删除排除用户失败', 'Failed to delete exclusion')))
  }
}

async function loadRuns(campaignId: number): Promise<void> {
  try {
    const result = await rankingRewardAPI.listRuns(campaignId, { page: 1, page_size: 50 })
    runs.value = result.items || []
    if (!selectedRun.value && runs.value.length > 0) {
      await selectRun(runs.value[0])
    }
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tx('加载运行记录失败', 'Failed to load runs')))
  }
}

async function selectRun(run: RankingRewardRun): Promise<void> {
  selectedRun.value = run
  try {
    const result = await rankingRewardAPI.listAwards(run.id, { page: 1, page_size: 200 })
    awards.value = result.items || []
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tx('加载发放明细失败', 'Failed to load awards')))
  }
}

async function confirmRun(campaign: RankingRewardCampaign, rewardDate = ''): Promise<void> {
  const dateLabel = rewardDate || tx('上一自然日', 'the previous local day')
  if (!window.confirm(tx(`确认执行“${campaign.name}”在 ${dateLabel} 的排行榜奖励？`, `Run ranking rewards for "${campaign.name}" on ${dateLabel}?`))) return
  await runNow(campaign, rewardDate)
}

async function runNow(campaign: RankingRewardCampaign, rewardDate = ''): Promise<void> {
  if (runningCampaignId.value !== null) return
  runningCampaignId.value = campaign.id
  try {
    const result = await rankingRewardAPI.runCampaign(
      campaign.id,
      { reward_date: dateInputToISO(rewardDate, campaign.timezone) },
      { idempotencyKey: createIdempotencyKey('run-campaign') }
    )
    appStore.showSuccess(tx('排行榜奖励执行完成', 'Ranking reward run completed'))
    selectedCampaign.value = campaign
    selectedRun.value = result.run
    awards.value = result.awards || []
    await loadCampaigns()
    await loadRuns(campaign.id)
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tx('执行排行榜奖励失败', 'Failed to run ranking reward campaign')))
  } finally {
    runningCampaignId.value = null
  }
}

onMounted(() => {
  loadCampaigns()
})
</script>
