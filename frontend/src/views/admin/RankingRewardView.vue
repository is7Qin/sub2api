<template>
  <AppLayout>
    <div class="space-y-6">
      <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div>
          <h1 class="text-2xl font-bold text-gray-900 dark:text-white">{{ t('admin.rankingReward.title') }}</h1>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.rankingReward.description') }}</p>
        </div>
        <button class="btn btn-primary" type="button" @click="openCreateCampaignDialog">
          <Icon name="plus" size="sm" />
          <span>{{ tr('newCampaign') }}</span>
        </button>
      </div>

      <div class="grid gap-4 sm:grid-cols-4">
        <div class="card p-5">
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ tr('stats.campaigns') }}</p>
          <p class="mt-2 text-2xl font-semibold text-gray-900 dark:text-white">{{ campaigns.length }}</p>
        </div>
        <div class="card p-5">
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ tr('stats.active') }}</p>
          <p class="mt-2 text-2xl font-semibold text-emerald-600 dark:text-emerald-400">{{ activeCampaignCount }}</p>
        </div>
        <div class="card p-5">
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ tr('stats.lastAwards') }}</p>
          <p class="mt-2 text-2xl font-semibold text-primary-600 dark:text-primary-400">{{ latestRun?.awarded_count ?? '-' }}</p>
        </div>
        <div class="card p-5">
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ tr('stats.awardedCost') }}</p>
          <p class="mt-2 text-2xl font-semibold text-amber-600 dark:text-amber-400">{{ latestRun ? formatMoney(latestRun.total_actual_cost) : '-' }}</p>
        </div>
      </div>

      <TablePageLayout>
        <template #filters>
          <div class="flex flex-wrap items-center gap-3">
            <div class="w-full sm:w-48">
              <Select v-model="statusFilter" :options="statusFilterOptions" @change="loadCampaigns" />
            </div>
            <button class="btn btn-secondary" :disabled="loadingCampaigns" type="button" @click="loadCampaigns">
              <Icon name="refresh" size="sm" :class="loadingCampaigns ? 'animate-spin' : ''" />
              <span>{{ t('common.refresh') }}</span>
            </button>
          </div>
        </template>

        <template #table>
          <DataTable
            :columns="campaignColumns"
            :data="campaigns"
            :loading="loadingCampaigns"
            row-key="id"
            sticky-actions-column
            :actions-count="5"
          >
            <template #cell-name="{ row }">
              <div class="min-w-0">
                <div class="flex flex-wrap items-center gap-2">
                  <span class="font-medium text-gray-900 dark:text-white">{{ row.name }}</span>
                  <span class="text-xs text-gray-400 dark:text-gray-500">#{{ row.id }}</span>
                </div>
                <p class="mt-1 max-w-xl truncate text-xs text-gray-500 dark:text-gray-400">{{ row.description || '-' }}</p>
              </div>
            </template>

            <template #cell-status="{ row }">
              <span class="rounded-full px-2 py-0.5 text-xs font-medium" :class="campaignStatusClass(row.status)">
                {{ statusLabel(row.status) }}
              </span>
            </template>

            <template #cell-lottery_campaign_id="{ row }">
              <div class="text-sm text-gray-700 dark:text-gray-300">
                {{ lotteryCampaignLabel(row.lottery_campaign_id) }}
              </div>
            </template>

            <template #cell-reward="{ row }">
              <div class="space-y-1 text-xs text-gray-600 dark:text-gray-300">
                <div>{{ tr('reward.winners') }}: {{ row.top_n }}</div>
                <div>{{ tr('reward.display') }}: {{ row.public_display_limit }}</div>
                <div>{{ tr('reward.chancesPerUser') }}: {{ row.chance_count }}</div>
              </div>
            </template>

            <template #cell-window="{ row }">
              <div class="space-y-1 text-xs text-gray-600 dark:text-gray-300">
                <div>{{ tr('fields.start') }}: {{ formatDateTime(row.starts_at) || '-' }}</div>
                <div>{{ tr('fields.end') }}: {{ formatDateTime(row.ends_at) || tr('noLimit') }}</div>
              </div>
            </template>

            <template #cell-actions="{ row }">
              <div class="flex justify-end gap-2">
                <button class="btn btn-sm btn-secondary" type="button" @click="openEditCampaignDialog(row)">{{ tr('actions.edit') }}</button>
                <button class="btn btn-sm btn-secondary" type="button" @click="openExclusionsDialog(row)">{{ tr('actions.exclusions') }}</button>
                <button class="btn btn-sm btn-secondary" type="button" @click="openRunsDialog(row)">{{ tr('actions.runs') }}</button>
                <button class="btn btn-sm btn-primary" :disabled="runningCampaignId === row.id" type="button" @click="confirmRun(row)">
                  <Icon v-if="runningCampaignId === row.id" name="refresh" size="xs" class="animate-spin" />
                  <span>{{ tr('actions.run') }}</span>
                </button>
              </div>
            </template>

            <template #empty>
              <div class="flex flex-col items-center py-8 text-center">
                <p class="text-base font-medium text-gray-900 dark:text-white">{{ tr('empty.campaignsTitle') }}</p>
                <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ tr('empty.campaignsDescription') }}</p>
                <button class="btn btn-primary mt-4" type="button" @click="openCreateCampaignDialog">
                  <Icon name="plus" size="sm" />
                  <span>{{ tr('newCampaign') }}</span>
                </button>
              </div>
            </template>
          </DataTable>
        </template>
      </TablePageLayout>
    </div>

    <BaseDialog
      :show="showCampaignDialog"
      :title="formMode === 'create' ? tr('dialogs.createCampaign') : tr('dialogs.editCampaign')"
      width="wide"
      @close="closeCampaignDialog"
    >
      <form id="ranking-reward-campaign-form" class="space-y-4" @submit.prevent="saveCampaign">
        <div>
          <label class="label">{{ tr('fields.name') }}</label>
          <input v-model.trim="campaignForm.name" class="input" required />
        </div>
        <div>
          <label class="label">{{ tr('fields.description') }}</label>
          <textarea v-model="campaignForm.description" class="input min-h-[80px]"></textarea>
        </div>
        <div class="grid gap-4 sm:grid-cols-2">
          <div>
            <label class="label">{{ tr('fields.status') }}</label>
            <Select v-model="campaignForm.status" :options="campaignStatusOptions" />
          </div>
          <div>
            <label class="label">{{ tr('fields.lotteryCampaign') }}</label>
            <Select
              v-model="campaignForm.lottery_campaign_id"
              :options="lotteryCampaignOptions"
              searchable
              :placeholder="tr('placeholders.selectLotteryCampaign')"
            />
          </div>
        </div>
        <div class="grid gap-4 sm:grid-cols-2">
          <div>
            <label class="label">{{ tr('fields.topN') }}</label>
            <input v-model.number="campaignForm.top_n" class="input" min="1" max="1000" type="number" />
          </div>
          <div>
            <label class="label">{{ tr('fields.publicDisplayTopN') }}</label>
            <input v-model.number="campaignForm.public_display_limit" class="input" min="1" max="1000" type="number" />
            <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ tr('hints.publicDisplayLimit') }}</p>
          </div>
          <div>
            <label class="label">{{ tr('fields.chancesPerUser') }}</label>
            <input v-model.number="campaignForm.chance_count" class="input" min="1" max="1000" type="number" />
          </div>
          <div>
            <label class="label">{{ tr('fields.minActualCost') }}</label>
            <input v-model.number="campaignForm.min_actual_cost" class="input" min="0" step="0.01" type="number" />
          </div>
        </div>
        <div class="grid gap-4 sm:grid-cols-2">
          <div>
            <label class="label">{{ tr('fields.startsAt') }}</label>
            <input v-model="campaignForm.starts_at" class="input" type="datetime-local" />
          </div>
          <div>
            <label class="label">{{ tr('fields.endsAt') }}</label>
            <input v-model="campaignForm.ends_at" class="input" type="datetime-local" />
          </div>
        </div>
        <div>
          <label class="label">{{ tr('fields.timezone') }}</label>
          <input v-model.trim="campaignForm.timezone" class="input" placeholder="Asia/Shanghai" />
        </div>
      </form>
      <template #footer>
        <button class="btn btn-secondary" type="button" @click="closeCampaignDialog">{{ t('common.cancel') }}</button>
        <button class="btn btn-primary" :disabled="savingCampaign" form="ranking-reward-campaign-form" type="submit">
          <Icon v-if="savingCampaign" name="refresh" size="sm" class="animate-spin" />
          <span>{{ t('common.save') }}</span>
        </button>
      </template>
    </BaseDialog>

    <BaseDialog
      :show="showExclusionsDialog"
      :title="activeCampaign ? `${tr('dialogs.excludedUsers')} · ${activeCampaign.name}` : tr('dialogs.excludedUsers')"
      width="extra-wide"
      @close="closeExclusionsDialog"
    >
      <div class="space-y-4">
        <form class="grid gap-3 lg:grid-cols-[minmax(260px,1fr)_minmax(220px,1fr)_auto]" @submit.prevent="addExclusion">
          <div>
            <label class="label">{{ tr('fields.selectUser') }}</label>
            <div class="relative">
              <input
                v-model.trim="userSearchQuery"
                class="input"
                :placeholder="tr('placeholders.searchUser')"
                autocomplete="off"
                required
                @input="handleUserSearch"
                @focus="showUserResults = userSearchResults.length > 0"
              />
              <div
                v-if="showUserResults"
                class="absolute z-50 mt-1 max-h-64 w-full overflow-y-auto rounded-lg border border-gray-200 bg-white shadow-lg dark:border-dark-700 dark:bg-dark-800"
              >
                <button
                  v-for="user in userSearchResults"
                  :key="user.id"
                  type="button"
                  class="flex w-full items-start justify-between gap-3 px-3 py-2 text-left hover:bg-gray-50 dark:hover:bg-dark-700"
                  @click="selectExclusionUser(user)"
                >
                  <span class="min-w-0">
                    <span class="block truncate text-sm font-medium text-gray-900 dark:text-white">{{ user.email }}</span>
                    <span class="block truncate text-xs text-gray-500 dark:text-gray-400">{{ user.username || user.notes || `#${user.id}` }}</span>
                  </span>
                  <span class="shrink-0 text-xs text-gray-400 dark:text-gray-500">#{{ user.id }}</span>
                </button>
                <div v-if="userSearchResults.length === 0 && !userSearchLoading" class="px-3 py-2 text-sm text-gray-500 dark:text-gray-400">
                  {{ tr('empty.noMatchingUsers') }}
                </div>
              </div>
            </div>
          </div>
          <div>
            <label class="label">{{ tr('fields.reason') }}</label>
            <input v-model="exclusionForm.reason" class="input" :placeholder="tr('fields.reason')" />
          </div>
          <div class="flex items-end">
            <button class="btn btn-primary w-full" :disabled="!selectedExclusionUser || savingExclusion" type="submit">
              <Icon v-if="savingExclusion" name="refresh" size="sm" class="animate-spin" />
              <span>{{ t('common.add') }}</span>
            </button>
          </div>
        </form>

        <div v-if="selectedExclusionUser" class="rounded-lg border border-primary-100 bg-primary-50 p-3 text-sm dark:border-primary-900/50 dark:bg-primary-900/20">
          <div class="font-medium text-primary-700 dark:text-primary-300">{{ selectedExclusionUser.email }}</div>
          <div class="mt-1 text-primary-600 dark:text-primary-400">{{ selectedExclusionUser.username || selectedExclusionUser.notes || tr('userSelected') }} · #{{ selectedExclusionUser.id }}</div>
        </div>

        <DataTable :columns="exclusionColumns" :data="exclusions" row-key="id" :actions-count="1">
          <template #cell-user_id="{ row }">
            <span>#{{ row.user_id }}</span>
          </template>
          <template #cell-created_at="{ row }">
            <span>{{ formatDateTime(row.created_at) }}</span>
          </template>
          <template #cell-actions="{ row }">
            <div class="flex justify-end">
              <button class="text-sm text-red-600 hover:underline dark:text-red-400" type="button" @click="removeExclusion(row.id)">{{ t('common.delete') }}</button>
            </div>
          </template>
          <template #empty>
            <div class="py-8 text-center text-sm text-gray-500 dark:text-gray-400">{{ tr('empty.noExcludedUsers') }}</div>
          </template>
        </DataTable>
      </div>
    </BaseDialog>

    <BaseDialog
      :show="showRunsDialog"
      :title="activeCampaign ? `${tr('actions.runs')} · ${activeCampaign.name}` : tr('actions.runs')"
      width="full"
      @close="closeRunsDialog"
    >
      <div v-if="activeCampaign" class="space-y-6">
        <div class="flex flex-col gap-3 rounded-lg bg-gray-50 p-4 dark:bg-dark-800 sm:flex-row sm:items-end sm:justify-between">
          <div>
            <p class="text-sm font-medium text-gray-900 dark:text-white">{{ tr('manualRun.title') }}</p>
            <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ tr('manualRun.description') }}</p>
          </div>
          <form class="flex gap-2" @submit.prevent="confirmRun(activeCampaign, runDate)">
            <input v-model="runDate" class="input w-40" type="date" />
            <button class="btn btn-primary" :disabled="runningCampaignId === activeCampaign.id" type="submit">
              <Icon v-if="runningCampaignId === activeCampaign.id" name="refresh" size="sm" class="animate-spin" />
              <span>{{ tr('actions.run') }}</span>
            </button>
          </form>
        </div>

        <DataTable :columns="runColumns" :data="runs" row-key="id" :actions-count="1">
          <template #cell-reward_date="{ row }">
            <span>{{ formatDateOnly(row.reward_date) }}</span>
          </template>
          <template #cell-status="{ row }">
            <span class="rounded-full px-2 py-0.5 text-xs font-medium" :class="runStatusClass(row.status)">{{ runStatusLabel(row.status) }}</span>
          </template>
          <template #cell-total_actual_cost="{ row }">
            <span>{{ formatMoney(row.total_actual_cost) }}</span>
          </template>
          <template #cell-finished_at="{ row }">
            <span>{{ formatDateTime(row.finished_at) || '-' }}</span>
          </template>
          <template #cell-actions="{ row }">
            <div class="flex justify-end">
              <button class="text-sm text-primary-600 hover:underline dark:text-primary-400" type="button" @click="selectRun(row)">{{ tr('actions.awards') }}</button>
            </div>
          </template>
          <template #empty>
            <div class="py-8 text-center text-sm text-gray-500 dark:text-gray-400">{{ tr('empty.noRuns') }}</div>
          </template>
        </DataTable>

        <div v-if="selectedRun" class="space-y-3">
          <div>
            <h3 class="text-base font-semibold text-gray-900 dark:text-white">{{ tr('awardDetails.title') }}</h3>
            <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ tr('awardDetails.runId') }} #{{ selectedRun.id }}</p>
          </div>
          <DataTable :columns="awardColumns" :data="awards" row-key="id">
            <template #cell-rank="{ row }">
              <span class="font-semibold">#{{ row.rank }}</span>
            </template>
            <template #cell-user_id="{ row }">
              <span>#{{ row.user_id }}</span>
            </template>
            <template #cell-actual_cost="{ row }">
              <span>{{ formatMoney(row.actual_cost) }}</span>
            </template>
            <template #cell-requests="{ row }">
              <span>{{ formatNumber(row.requests) }}</span>
            </template>
            <template #cell-tokens="{ row }">
              <span>{{ formatNumber(row.tokens) }}</span>
            </template>
            <template #empty>
              <div class="py-8 text-center text-sm text-gray-500 dark:text-gray-400">{{ tr('empty.noAwards') }}</div>
            </template>
          </DataTable>
        </div>
      </div>
    </BaseDialog>

    <ConfirmDialog
      :show="showRunConfirmDialog"
      :title="tr('confirmRun.title')"
      :message="runConfirmMessage"
      :confirm-text="tr('actions.run')"
      :cancel-text="t('common.cancel')"
      @confirm="confirmRunDialog"
      @cancel="closeRunConfirmDialog"
    />
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import DataTable from '@/components/common/DataTable.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import Select, { type SelectOption } from '@/components/common/Select.vue'
import Icon from '@/components/icons/Icon.vue'
import { adminAPI } from '@/api/admin'
import rankingRewardAPI from '@/api/admin/rankingReward'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatCurrency, formatDateOnly, formatDateTime, formatNumber, formatDateTimeLocalInput } from '@/utils/format'
import type { AdminUser } from '@/types'
import type { Column } from '@/components/common/types'
import type { LotteryCampaign } from '@/api/admin/lottery'
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

const { t } = useI18n()
const appStore = useAppStore()

const loadingCampaigns = ref(false)
const loadingLotteryCampaigns = ref(false)
const savingCampaign = ref(false)
const savingExclusion = ref(false)
const runningCampaignId = ref<number | null>(null)
const statusFilter = ref<string | null>('')
const campaigns = ref<RankingRewardCampaign[]>([])
const lotteryCampaigns = ref<LotteryCampaign[]>([])
const activeCampaign = ref<RankingRewardCampaign | null>(null)
const exclusions = ref<RankingRewardExclusion[]>([])
const runs = ref<RankingRewardRun[]>([])
const selectedRun = ref<RankingRewardRun | null>(null)
const awards = ref<RankingRewardAward[]>([])
const latestRun = ref<RankingRewardRun | null>(null)
const formMode = ref<'create' | 'edit'>('create')
const editingCampaignId = ref<number | null>(null)
const runDate = ref('')
const showCampaignDialog = ref(false)
const showExclusionsDialog = ref(false)
const showRunsDialog = ref(false)
const showRunConfirmDialog = ref(false)
const pendingRunCampaign = ref<RankingRewardCampaign | null>(null)
const pendingRunDate = ref('')
const userSearchQuery = ref('')
const userSearchResults = ref<AdminUser[]>([])
const userSearchLoading = ref(false)
const showUserResults = ref(false)
const selectedExclusionUser = ref<AdminUser | null>(null)
let userSearchTimeout: ReturnType<typeof setTimeout> | null = null

const activeCampaignCount = computed(() => campaigns.value.filter((item) => item.status === 'active').length)
const runConfirmMessage = computed(() => {
  const campaign = pendingRunCampaign.value
  const dateLabel = pendingRunDate.value || tr('previousLocalDay')
  return campaign ? tr('confirmRun.message', { name: campaign.name, date: dateLabel }) : ''
})

const campaignForm = reactive({
  name: '',
  description: '',
  status: 'draft' as RankingRewardCampaignStatus,
  lottery_campaign_id: null as number | null,
  top_n: 10,
  chance_count: 1,
  public_display_limit: 10,
  min_actual_cost: 0,
  starts_at: '',
  ends_at: '',
  timezone: 'Asia/Shanghai',
})

const exclusionForm = reactive({
  reason: '',
})

const campaignColumns = computed<Column[]>(() => [
  { key: 'name', label: tr('fields.name'), sortable: true },
  { key: 'status', label: tr('fields.status'), sortable: true },
  { key: 'lottery_campaign_id', label: tr('fields.lotteryCampaign') },
  { key: 'reward', label: tr('columns.rewardRule') },
  { key: 'min_actual_cost', label: tr('columns.minCost'), formatter: (value) => formatMoney(Number(value)) },
  { key: 'last_run_date', label: tr('columns.lastRun'), formatter: (value) => formatDateOnly(value) || '-' },
  { key: 'window', label: tr('columns.window') },
  { key: 'actions', label: t('common.actions'), class: 'text-right' },
])

const exclusionColumns = computed<Column[]>(() => [
  { key: 'user_id', label: tr('columns.user') },
  { key: 'reason', label: tr('fields.reason'), formatter: (value) => value || '-' },
  { key: 'created_at', label: tr('columns.created') },
  { key: 'actions', label: t('common.actions'), class: 'text-right' },
])

const runColumns = computed<Column[]>(() => [
  { key: 'reward_date', label: tr('columns.rewardDate'), sortable: true },
  { key: 'status', label: tr('fields.status'), sortable: true },
  { key: 'awarded_count', label: tr('columns.awards'), sortable: true },
  { key: 'total_actual_cost', label: tr('columns.cost'), sortable: true },
  { key: 'finished_at', label: tr('columns.finished') },
  { key: 'actions', label: t('common.actions'), class: 'text-right' },
])

const awardColumns = computed<Column[]>(() => [
  { key: 'rank', label: tr('columns.rank'), sortable: true },
  { key: 'user_id', label: tr('columns.user') },
  { key: 'actual_cost', label: tr('columns.actualCost'), sortable: true },
  { key: 'requests', label: tr('columns.requests'), sortable: true },
  { key: 'tokens', label: 'Tokens', sortable: true },
  { key: 'chance_count', label: tr('columns.chances'), sortable: true },
])

const statusFilterOptions = computed<SelectOption[]>(() => [
  { value: '', label: tr('filters.allStatus') },
  { value: 'draft', label: statusLabel('draft') },
  { value: 'active', label: statusLabel('active') },
  { value: 'disabled', label: statusLabel('disabled') },
  { value: 'ended', label: statusLabel('ended') },
])

const campaignStatusOptions = computed<SelectOption[]>(() => [
  { value: 'draft', label: statusLabel('draft') },
  { value: 'active', label: statusLabel('active') },
  { value: 'disabled', label: statusLabel('disabled') },
  { value: 'ended', label: statusLabel('ended') },
])

const lotteryCampaignOptions = computed<SelectOption[]>(() =>
  lotteryCampaigns.value.map((campaign) => ({
    value: campaign.id,
    label: `${campaign.name} (#${campaign.id})`,
    description: `${lotteryStatusLabel(campaign.status)} · ${formatDateTime(campaign.starts_at) || '-'} - ${formatDateTime(campaign.ends_at) || tr('noLimit')}`,
  }))
)

function tr(key: string, params?: Record<string, unknown>): string {
  return params ? t(`admin.rankingReward.${key}`, params) : t(`admin.rankingReward.${key}`)
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
  const labels: Record<RankingRewardCampaignStatus, string> = {
    draft: tr('status.draft'),
    active: tr('status.active'),
    disabled: tr('status.disabled'),
    ended: tr('status.ended'),
  }
  return labels[status] || status
}

function lotteryStatusLabel(status: string): string {
  const labels: Record<string, string> = {
    draft: tr('status.draft'),
    active: tr('lotteryStatus.active'),
    disabled: tr('lotteryStatus.disabled'),
    ended: tr('status.ended'),
  }
  return labels[status] || status
}

function runStatusLabel(status: RankingRewardRunStatus): string {
  const labels: Record<RankingRewardRunStatus, string> = {
    running: tr('runStatus.running'),
    completed: tr('runStatus.completed'),
    failed: tr('runStatus.failed'),
  }
  return labels[status] || status
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

function lotteryCampaignLabel(id: number): string {
  const campaign = lotteryCampaigns.value.find((item) => item.id === id)
  return campaign ? `${campaign.name} (#${campaign.id})` : `#${id}`
}

function resetCampaignForm(): void {
  Object.assign(campaignForm, {
    name: '',
    description: '',
    status: 'draft',
    lottery_campaign_id: null,
    top_n: 10,
    chance_count: 1,
    public_display_limit: 10,
    min_actual_cost: 0,
    starts_at: '',
    ends_at: '',
    timezone: 'Asia/Shanghai',
  })
}

function resetExclusionForm(): void {
  exclusionForm.reason = ''
  userSearchQuery.value = ''
  userSearchResults.value = []
  showUserResults.value = false
  selectedExclusionUser.value = null
}

function openCreateCampaignDialog(): void {
  formMode.value = 'create'
  editingCampaignId.value = null
  resetCampaignForm()
  showCampaignDialog.value = true
  loadLotteryCampaigns()
}

function openEditCampaignDialog(campaign: RankingRewardCampaign): void {
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
  showCampaignDialog.value = true
  loadLotteryCampaigns()
}

function closeCampaignDialog(): void {
  showCampaignDialog.value = false
}

function buildCampaignPayload(): CreateRankingRewardCampaignRequest | null {
  const topN = Number(campaignForm.top_n)
  const chanceCount = Number(campaignForm.chance_count)
  const publicDisplayLimit = Number(campaignForm.public_display_limit)
  if (!campaignForm.lottery_campaign_id) {
    appStore.showError(tr('errors.selectLotteryCampaign'))
    return null
  }
  if (!Number.isFinite(topN) || !Number.isFinite(chanceCount) || !Number.isFinite(publicDisplayLimit) || topN <= 0 || chanceCount <= 0 || publicDisplayLimit <= 0) {
    appStore.showError(tr('errors.positiveRewardNumbers'))
    return null
  }
  if (topN > MAX_RANKING_REWARD_TOP_N || chanceCount > MAX_RANKING_REWARD_CHANCE_COUNT || publicDisplayLimit > MAX_RANKING_REWARD_PUBLIC_DISPLAY_LIMIT) {
    appStore.showError(tr('errors.rewardNumbersMax'))
    return null
  }
  if (topN * chanceCount > MAX_TOTAL_CHANCES) {
    appStore.showError(tr('errors.totalChancesMax'))
    return null
  }
  const payload: CreateRankingRewardCampaignRequest = {
    name: campaignForm.name.trim(),
    description: campaignForm.description,
    status: campaignForm.status,
    lottery_campaign_id: campaignForm.lottery_campaign_id,
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
    await updateLatestRunPreview()
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tr('errors.loadRankingRewards')))
  } finally {
    loadingCampaigns.value = false
  }
}

async function loadLotteryCampaigns(): Promise<void> {
  if (loadingLotteryCampaigns.value || lotteryCampaigns.value.length > 0) return
  loadingLotteryCampaigns.value = true
  try {
    const result = await adminAPI.lottery.listCampaigns({ page: 1, page_size: 200 })
    lotteryCampaigns.value = result.items || []
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tr('errors.loadLotteryCampaigns')))
  } finally {
    loadingLotteryCampaigns.value = false
  }
}

async function updateLatestRunPreview(): Promise<void> {
  const first = campaigns.value[0]
  if (!first) {
    latestRun.value = null
    return
  }
  try {
    const result = await rankingRewardAPI.listRuns(first.id, { page: 1, page_size: 1 })
    latestRun.value = result.items?.[0] || null
  } catch {
    latestRun.value = null
  }
}

async function saveCampaign(): Promise<void> {
  if (!campaignForm.name.trim()) return
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
    appStore.showSuccess(tr('messages.campaignSaved'))
    showCampaignDialog.value = false
    await loadCampaigns()
    activeCampaign.value = item
    if (formMode.value === 'create') resetCampaignForm()
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tr('errors.saveCampaign')))
  } finally {
    savingCampaign.value = false
  }
}

async function openExclusionsDialog(campaign: RankingRewardCampaign): Promise<void> {
  activeCampaign.value = campaign
  resetExclusionForm()
  showExclusionsDialog.value = true
  await loadExclusions(campaign.id)
}

function closeExclusionsDialog(): void {
  showExclusionsDialog.value = false
  resetExclusionForm()
}

async function loadExclusions(campaignId: number): Promise<void> {
  try {
    const result = await rankingRewardAPI.listExclusions(campaignId, { page: 1, page_size: 100 })
    exclusions.value = result.items || []
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tr('errors.loadExclusions')))
  }
}

function handleUserSearch(): void {
  if (userSearchTimeout) clearTimeout(userSearchTimeout)
  selectedExclusionUser.value = null
  const query = userSearchQuery.value.trim()
  if (query.length < 2) {
    userSearchResults.value = []
    showUserResults.value = false
    return
  }
  userSearchTimeout = setTimeout(async () => {
    userSearchLoading.value = true
    try {
      const result = await adminAPI.users.list(1, 10, { search: query })
      userSearchResults.value = result.items || []
      showUserResults.value = true
    } catch {
      userSearchResults.value = []
      showUserResults.value = false
    } finally {
      userSearchLoading.value = false
    }
  }, 300)
}

function selectExclusionUser(user: AdminUser): void {
  selectedExclusionUser.value = user
  userSearchQuery.value = user.email
  userSearchResults.value = []
  showUserResults.value = false
}

async function addExclusion(): Promise<void> {
  if (!activeCampaign.value || !selectedExclusionUser.value) return
  savingExclusion.value = true
  try {
    await rankingRewardAPI.createExclusion(activeCampaign.value.id, {
      user_id: selectedExclusionUser.value.id,
      reason: exclusionForm.reason,
    }, { idempotencyKey: createIdempotencyKey('create-exclusion') })
    resetExclusionForm()
    await loadExclusions(activeCampaign.value.id)
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tr('errors.addExclusion')))
  } finally {
    savingExclusion.value = false
  }
}

async function removeExclusion(id: number): Promise<void> {
  if (!activeCampaign.value) return
  try {
    await rankingRewardAPI.deleteExclusion(id)
    await loadExclusions(activeCampaign.value.id)
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tr('errors.deleteExclusion')))
  }
}

async function openRunsDialog(campaign: RankingRewardCampaign): Promise<void> {
  activeCampaign.value = campaign
  selectedRun.value = null
  awards.value = []
  runDate.value = ''
  showRunsDialog.value = true
  await loadRuns(campaign.id)
}

function closeRunsDialog(): void {
  showRunsDialog.value = false
}

async function loadRuns(campaignId: number): Promise<void> {
  try {
    const result = await rankingRewardAPI.listRuns(campaignId, { page: 1, page_size: 50 })
    runs.value = result.items || []
    if (runs.value.length > 0) await selectRun(runs.value[0])
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tr('errors.loadRuns')))
  }
}

async function selectRun(run: RankingRewardRun): Promise<void> {
  selectedRun.value = run
  try {
    const result = await rankingRewardAPI.listAwards(run.id, { page: 1, page_size: 200 })
    awards.value = result.items || []
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tr('errors.loadAwards')))
  }
}

function confirmRun(campaign: RankingRewardCampaign, rewardDate = ''): void {
  pendingRunCampaign.value = campaign
  pendingRunDate.value = rewardDate
  showRunConfirmDialog.value = true
}

function closeRunConfirmDialog(): void {
  showRunConfirmDialog.value = false
  pendingRunCampaign.value = null
  pendingRunDate.value = ''
}

async function confirmRunDialog(): Promise<void> {
  const campaign = pendingRunCampaign.value
  const rewardDate = pendingRunDate.value
  closeRunConfirmDialog()
  if (!campaign) return
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
    appStore.showSuccess(tr('messages.runCompleted'))
    activeCampaign.value = campaign
    selectedRun.value = result.run
    latestRun.value = result.run
    awards.value = result.awards || []
    await loadCampaigns()
    if (showRunsDialog.value) await loadRuns(campaign.id)
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tr('errors.runCampaign')))
  } finally {
    runningCampaignId.value = null
  }
}

onMounted(() => {
  loadCampaigns()
  loadLotteryCampaigns()
})

onUnmounted(() => {
  if (userSearchTimeout) clearTimeout(userSearchTimeout)
})
</script>
