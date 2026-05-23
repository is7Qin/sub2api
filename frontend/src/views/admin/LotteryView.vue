<template>
  <AppLayout>
    <div class="space-y-6">
      <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div>
          <h1 class="text-2xl font-bold text-gray-900 dark:text-white">{{ tx('抽奖中心', 'Lottery') }}</h1>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
            {{ tx('管理抽奖活动、奖品和用户抽奖机会。', 'Manage lottery campaigns, prizes and user chances.') }}
          </p>
        </div>
        <div class="flex gap-2">
          <select v-model="statusFilter" class="input w-36" @change="loadCampaigns()">
            <option value="">{{ tx('全部状态', 'All status') }}</option>
            <option value="draft">{{ statusLabel('draft') }}</option>
            <option value="active">{{ statusLabel('active') }}</option>
            <option value="disabled">{{ statusLabel('disabled') }}</option>
            <option value="ended">{{ statusLabel('ended') }}</option>
          </select>
          <button class="btn btn-secondary" :disabled="loading" @click="loadCampaigns()">
            <Icon name="refresh" size="sm" :class="loading ? 'animate-spin' : ''" />
            <span>{{ tx('刷新', 'Refresh') }}</span>
          </button>
          <button class="btn btn-primary" @click="resetCampaignForm">
            <Icon name="plus" size="sm" />
            <span>{{ tx('新建活动', 'New Campaign') }}</span>
          </button>
        </div>
      </div>

      <div class="grid gap-6 xl:grid-cols-[minmax(0,1fr)_420px]">
        <div class="space-y-6">
          <div class="card overflow-hidden">
            <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
              <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ tx('活动列表', 'Campaigns') }}</h2>
            </div>
            <div v-if="loading" class="flex justify-center py-12">
              <div class="h-8 w-8 animate-spin rounded-full border-2 border-primary-500 border-t-transparent"></div>
            </div>
            <div v-else-if="campaigns.length === 0" class="p-8 text-center text-sm text-gray-500 dark:text-gray-400">
              {{ tx('暂无抽奖活动。', 'No lottery campaigns yet.') }}
            </div>
            <div v-else class="divide-y divide-gray-100 dark:divide-dark-700">
              <div
                v-for="campaign in campaigns"
                :key="campaign.id"
                class="cursor-pointer p-5 transition hover:bg-gray-50 dark:hover:bg-dark-800/60"
                :class="selectedCampaignId === campaign.id ? 'bg-primary-50/60 dark:bg-primary-900/20' : ''"
                @click="selectCampaign(campaign.id)"
              >
                <div class="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
                  <div class="min-w-0">
                    <div class="flex flex-wrap items-center gap-2">
                      <h3 class="truncate text-base font-semibold text-gray-900 dark:text-white">{{ campaign.name }}</h3>
                      <span class="rounded-full px-2 py-0.5 text-xs font-medium" :class="statusClass(campaign.status)">
                        {{ statusLabel(campaign.status) }}
                      </span>
                    </div>
                    <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ campaign.description || tx('无描述', 'No description') }}</p>
                    <div class="mt-3 grid gap-2 text-xs text-gray-500 dark:text-gray-400 sm:grid-cols-3">
                      <span>{{ tx('开始', 'Start') }}: {{ formatDateTime(campaign.starts_at) || '-' }}</span>
                      <span>{{ tx('结束', 'End') }}: {{ formatDateTime(campaign.ends_at) || tx('不限', 'No limit') }}</span>
                      <span>{{ tx('机会有效期', 'Chance TTL') }}: {{ campaign.chance_expires_in_days }} {{ tx('天', 'days') }}</span>
                    </div>
                  </div>
                  <div class="flex shrink-0 gap-2">
                    <button class="btn btn-secondary btn-sm" @click.stop="editCampaign(campaign)">
                      <Icon name="edit" size="sm" />
                      <span>{{ tx('编辑', 'Edit') }}</span>
                    </button>
                  </div>
                </div>
              </div>
            </div>
            <div v-if="campaigns.length > 0" class="flex items-center justify-between border-t border-gray-100 px-6 py-3 text-sm dark:border-dark-700">
              <span class="text-gray-500 dark:text-gray-400">{{ tx('共', 'Total') }} {{ pagination.total }}</span>
              <div class="flex items-center gap-2">
                <button class="btn btn-secondary btn-sm" :disabled="pagination.page <= 1" @click="changePage(pagination.page - 1)">
                  {{ tx('上一页', 'Prev') }}
                </button>
                <span class="text-gray-600 dark:text-gray-300">{{ pagination.page }} / {{ Math.max(1, pagination.pages) }}</span>
                <button class="btn btn-secondary btn-sm" :disabled="pagination.page >= pagination.pages" @click="changePage(pagination.page + 1)">
                  {{ tx('下一页', 'Next') }}
                </button>
              </div>
            </div>
          </div>

          <div v-if="selectedCampaign" class="card overflow-hidden">
            <div class="flex flex-col gap-3 border-b border-gray-100 px-6 py-4 dark:border-dark-700 sm:flex-row sm:items-center sm:justify-between">
              <div>
                <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ tx('奖品配置', 'Prizes') }}</h2>
                <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ selectedCampaign.name }}</p>
              </div>
              <button class="btn btn-secondary btn-sm" :disabled="prizesLoading" @click="loadPrizes(selectedCampaign.id)">
                <Icon name="refresh" size="sm" :class="prizesLoading ? 'animate-spin' : ''" />
                <span>{{ tx('刷新奖品', 'Refresh prizes') }}</span>
              </button>
            </div>
            <div v-if="prizesLoading" class="flex justify-center py-8">
              <div class="h-6 w-6 animate-spin rounded-full border-2 border-primary-500 border-t-transparent"></div>
            </div>
            <div v-else-if="prizes.length === 0" class="p-6 text-center text-sm text-gray-500 dark:text-gray-400">
              {{ tx('此活动还没有奖品。', 'No prizes for this campaign.') }}
            </div>
            <div v-else class="overflow-x-auto">
              <table class="w-full min-w-[760px] text-left text-sm">
                <thead>
                  <tr class="border-b border-gray-100 text-gray-500 dark:border-dark-700 dark:text-gray-400">
                    <th class="px-6 py-3 font-medium">{{ tx('奖品', 'Prize') }}</th>
                    <th class="px-4 py-3 font-medium">{{ tx('状态', 'Status') }}</th>
                    <th class="px-4 py-3 font-medium">{{ tx('权重', 'Weight') }}</th>
                    <th class="px-4 py-3 font-medium">{{ tx('库存', 'Stock') }}</th>
                    <th class="px-4 py-3 font-medium">{{ tx('奖励', 'Reward') }}</th>
                    <th class="px-6 py-3 text-right font-medium">{{ tx('操作', 'Actions') }}</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="prize in prizes" :key="prize.id" class="border-b border-gray-100 last:border-b-0 dark:border-dark-800">
                    <td class="px-6 py-4">
                      <div class="font-medium text-gray-900 dark:text-white">{{ prize.name }}</div>
                      <div class="text-xs text-gray-500 dark:text-gray-400">{{ prize.description || '-' }}</div>
                    </td>
                    <td class="px-4 py-4">
                      <span class="rounded-full px-2 py-0.5 text-xs font-medium" :class="prizeStatusClass(prize.status)">
                        {{ prizeStatusLabel(prize.status) }}
                      </span>
                    </td>
                    <td class="px-4 py-4 text-gray-700 dark:text-gray-300">{{ prize.weight }}</td>
                    <td class="px-4 py-4 text-gray-700 dark:text-gray-300">{{ prize.stock_used }} / {{ prize.stock_total }}</td>
                    <td class="px-4 py-4 text-gray-700 dark:text-gray-300">{{ rewardLabel(prize) }}</td>
                    <td class="px-6 py-4 text-right">
                      <button class="btn btn-secondary btn-sm" @click="editPrize(prize)">{{ tx('编辑', 'Edit') }}</button>
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
          </div>
        </div>

        <div class="space-y-6">
          <div class="card p-6">
            <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
              {{ campaignForm.id ? tx('编辑活动', 'Edit Campaign') : tx('新建活动', 'New Campaign') }}
            </h2>
            <form class="mt-4 space-y-4" @submit.prevent="saveCampaign">
              <div>
                <label class="input-label">{{ tx('名称', 'Name') }}</label>
                <input v-model.trim="campaignForm.name" class="input" required />
              </div>
              <div>
                <label class="input-label">{{ tx('描述', 'Description') }}</label>
                <textarea v-model.trim="campaignForm.description" class="input min-h-[80px]"></textarea>
              </div>
              <div class="grid grid-cols-2 gap-3">
                <div>
                  <label class="input-label">{{ tx('状态', 'Status') }}</label>
                  <select v-model="campaignForm.status" class="input">
                    <option value="draft">{{ statusLabel('draft') }}</option>
                    <option value="active">{{ statusLabel('active') }}</option>
                    <option value="disabled">{{ statusLabel('disabled') }}</option>
                    <option value="ended">{{ statusLabel('ended') }}</option>
                  </select>
                </div>
                <div>
                  <label class="input-label">{{ tx('机会有效期（天）', 'Chance TTL (days)') }}</label>
                  <input v-model.number="campaignForm.chance_expires_in_days" type="number" min="1" max="3650" class="input" />
                </div>
              </div>
              <div>
                <label class="input-label">{{ tx('开始时间', 'Starts at') }}</label>
                <input v-model="campaignForm.starts_at" type="datetime-local" class="input" />
              </div>
              <div>
                <label class="input-label">{{ tx('结束时间', 'Ends at') }}</label>
                <input v-model="campaignForm.ends_at" type="datetime-local" class="input" />
              </div>
              <div class="flex gap-2">
                <button class="btn btn-primary flex-1" :disabled="campaignSaving" type="submit">
                  <Icon v-if="campaignSaving" name="refresh" size="sm" class="animate-spin" />
                  <span>{{ campaignForm.id ? tx('保存活动', 'Save Campaign') : tx('创建活动', 'Create Campaign') }}</span>
                </button>
                <button class="btn btn-secondary" type="button" @click="resetCampaignForm">{{ tx('重置', 'Reset') }}</button>
              </div>
            </form>
          </div>

          <div class="card p-6" :class="!selectedCampaign ? 'opacity-60' : ''">
            <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
              {{ prizeForm.id ? tx('编辑奖品', 'Edit Prize') : tx('添加奖品', 'Add Prize') }}
            </h2>
            <p v-if="!selectedCampaign" class="mt-1 text-sm text-amber-600 dark:text-amber-400">
              {{ tx('请先选择一个活动。', 'Select a campaign first.') }}
            </p>
            <form class="mt-4 space-y-4" @submit.prevent="savePrize">
              <div>
                <label class="input-label">{{ tx('奖品名称', 'Prize name') }}</label>
                <input v-model.trim="prizeForm.name" class="input" required :disabled="!selectedCampaign" />
              </div>
              <div>
                <label class="input-label">{{ tx('描述', 'Description') }}</label>
                <textarea v-model.trim="prizeForm.description" class="input min-h-[70px]" :disabled="!selectedCampaign"></textarea>
              </div>
              <div class="grid grid-cols-2 gap-3">
                <div>
                  <label class="input-label">{{ tx('状态', 'Status') }}</label>
                  <select v-model="prizeForm.status" class="input" :disabled="!selectedCampaign">
                    <option value="active">{{ prizeStatusLabel('active') }}</option>
                    <option value="disabled">{{ prizeStatusLabel('disabled') }}</option>
                  </select>
                </div>
                <div>
                  <label class="input-label">{{ tx('奖励类型', 'Reward type') }}</label>
                  <select v-model="prizeForm.redeem_type" class="input" :disabled="!selectedCampaign">
                    <option value="balance">{{ tx('余额', 'Balance') }}</option>
                    <option value="concurrency">{{ tx('并发', 'Concurrency') }}</option>
                    <option value="subscription">{{ tx('订阅', 'Subscription') }}</option>
                    <option value="invitation">{{ tx('邀请码', 'Invitation') }}</option>
                    <option value="timed_quota">{{ tx('限时额度', 'Timed Quota') }}</option>
                    <option value="random_timed_quota">{{ tx('随机限时额度', 'Random Timed Quota') }}</option>
                  </select>
                </div>
              </div>
              <div class="grid grid-cols-2 gap-3">
                <div>
                  <label class="input-label">{{ tx('权重', 'Weight') }}</label>
                  <input v-model.number="prizeForm.weight" type="number" min="0" class="input" :disabled="!selectedCampaign" />
                </div>
                <div>
                  <label class="input-label">{{ tx('总库存', 'Total stock') }}</label>
                  <input v-model.number="prizeForm.stock_total" type="number" min="0" class="input" :disabled="!selectedCampaign" />
                </div>
              </div>
              <div v-if="prizeForm.redeem_type !== 'invitation' && prizeForm.redeem_type !== 'random_timed_quota'">
                <label class="input-label">{{ tx('奖励值', 'Reward value') }}</label>
                <input v-model.number="prizeForm.redeem_value" type="number" min="0" step="0.01" class="input" :disabled="!selectedCampaign" />
              </div>
              <div v-if="prizeForm.redeem_type === 'random_timed_quota'" class="grid grid-cols-2 gap-3">
                <div>
                  <label class="input-label">{{ tx('最小额度', 'Min value') }}</label>
                  <input v-model.number="prizeForm.min_value" type="number" min="0" step="0.01" class="input" :disabled="!selectedCampaign" />
                </div>
                <div>
                  <label class="input-label">{{ tx('最大额度', 'Max value') }}</label>
                  <input v-model.number="prizeForm.max_value" type="number" min="0" step="0.01" class="input" :disabled="!selectedCampaign" />
                </div>
              </div>
              <div v-if="prizeForm.redeem_type === 'subscription'">
                <label class="input-label">{{ tx('订阅分组 ID', 'Subscription group ID') }}</label>
                <input v-model.trim="prizeForm.redeem_group_id" type="number" min="1" class="input" :disabled="!selectedCampaign" />
              </div>
              <div v-if="['subscription', 'timed_quota', 'random_timed_quota'].includes(prizeForm.redeem_type)">
                <label class="input-label">{{ tx('有效天数', 'Validity days') }}</label>
                <input v-model.number="prizeForm.redeem_validity_days" type="number" min="1" max="3650" class="input" :disabled="!selectedCampaign" />
              </div>
              <div>
                <label class="input-label">{{ tx('排序', 'Sort order') }}</label>
                <input v-model.number="prizeForm.sort_order" type="number" class="input" :disabled="!selectedCampaign" />
              </div>
              <div class="flex gap-2">
                <button class="btn btn-primary flex-1" :disabled="!selectedCampaign || prizeSaving" type="submit">
                  <Icon v-if="prizeSaving" name="refresh" size="sm" class="animate-spin" />
                  <span>{{ prizeForm.id ? tx('保存奖品', 'Save Prize') : tx('添加奖品', 'Add Prize') }}</span>
                </button>
                <button class="btn btn-secondary" type="button" @click="resetPrizeForm">{{ tx('重置', 'Reset') }}</button>
              </div>
            </form>
          </div>

          <div class="card p-6" :class="!selectedCampaign ? 'opacity-60' : ''">
            <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ tx('发放抽奖机会', 'Grant Chances') }}</h2>
            <form class="mt-4 space-y-4" @submit.prevent="grantChances">
              <div>
                <label class="input-label">{{ tx('用户 ID', 'User ID') }}</label>
                <input v-model.trim="grantForm.user_id" type="number" min="1" class="input" required :disabled="!selectedCampaign" />
              </div>
              <div class="grid grid-cols-2 gap-3">
                <div>
                  <label class="input-label">{{ tx('数量', 'Count') }}</label>
                  <input v-model.number="grantForm.count" type="number" min="1" max="1000" class="input" :disabled="!selectedCampaign" />
                </div>
                <div>
                  <label class="input-label">{{ tx('来源', 'Source') }}</label>
                  <input v-model.trim="grantForm.source" class="input" :disabled="!selectedCampaign" />
                </div>
              </div>
              <div>
                <label class="input-label">{{ tx('来源 ID', 'Source ID') }}</label>
                <input v-model.trim="grantForm.source_id" class="input" :disabled="!selectedCampaign" />
              </div>
              <div>
                <label class="input-label">{{ tx('过期时间', 'Expires at') }}</label>
                <input v-model="grantForm.expires_at" type="datetime-local" class="input" :disabled="!selectedCampaign" />
              </div>
              <button class="btn btn-primary w-full" :disabled="!selectedCampaign || granting" type="submit">
                <Icon v-if="granting" name="refresh" size="sm" class="animate-spin" />
                <span>{{ tx('发放机会', 'Grant Chances') }}</span>
              </button>
            </form>
          </div>
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
import { useAppStore } from '@/stores/app'
import { formatCurrency, formatDateTime } from '@/utils/format'
import { extractApiErrorMessage } from '@/utils/apiError'
import lotteryAPI from '@/api/admin/lottery'
import type {
  CreateLotteryCampaignRequest,
  CreateLotteryPrizeRequest,
  LotteryCampaign,
  LotteryCampaignStatus,
  LotteryPrize,
  LotteryPrizeStatus,
  LotteryRedeemType,
  UpdateLotteryCampaignRequest,
  UpdateLotteryPrizeRequest,
} from '@/api/admin/lottery'

const { locale } = useI18n()
const appStore = useAppStore()

const loading = ref(false)
const prizesLoading = ref(false)
const campaignSaving = ref(false)
const prizeSaving = ref(false)
const granting = ref(false)
const campaigns = ref<LotteryCampaign[]>([])
const prizes = ref<LotteryPrize[]>([])
const statusFilter = ref('')
const selectedCampaignId = ref<number | null>(null)
const pagination = reactive({ page: 1, page_size: 20, total: 0, pages: 1 })

const selectedCampaign = computed(() => campaigns.value.find((item) => item.id === selectedCampaignId.value) || null)

const campaignForm = reactive({
  id: null as number | null,
  name: '',
  description: '',
  status: 'draft' as LotteryCampaignStatus,
  starts_at: '',
  ends_at: '',
  chance_expires_in_days: 30,
})

const prizeForm = reactive({
  id: null as number | null,
  name: '',
  description: '',
  status: 'active' as LotteryPrizeStatus,
  weight: 1,
  stock_total: 1,
  redeem_type: 'balance' as LotteryRedeemType,
  redeem_value: 0,
  redeem_group_id: '',
  redeem_validity_days: 30,
  min_value: 1,
  max_value: 10,
  sort_order: 0,
})

const grantForm = reactive({
  user_id: '',
  count: 1,
  source: 'admin',
  source_id: '',
  expires_at: '',
})

function tx(zh: string, en: string): string {
  return String(locale.value).startsWith('zh') ? zh : en
}

function statusLabel(status: LotteryCampaignStatus | string): string {
  const labels: Record<string, string> = {
    draft: tx('草稿', 'Draft'),
    active: tx('进行中', 'Active'),
    disabled: tx('已禁用', 'Disabled'),
    ended: tx('已结束', 'Ended'),
  }
  return labels[status] || status
}

function prizeStatusLabel(status: LotteryPrizeStatus | string): string {
  return status === 'active' ? tx('启用', 'Active') : tx('禁用', 'Disabled')
}

function statusClass(status: string): string {
  if (status === 'active') return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300'
  if (status === 'disabled') return 'bg-gray-100 text-gray-700 dark:bg-dark-700 dark:text-gray-300'
  if (status === 'ended') return 'bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-300'
  return 'bg-blue-100 text-blue-700 dark:bg-blue-900/40 dark:text-blue-300'
}

function prizeStatusClass(status: string): string {
  return status === 'active'
    ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300'
    : 'bg-gray-100 text-gray-700 dark:bg-dark-700 dark:text-gray-300'
}

function toLocalInput(value: string | null | undefined): string {
  if (!value) return ''
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return ''
  const offset = date.getTimezoneOffset() * 60000
  return new Date(date.getTime() - offset).toISOString().slice(0, 16)
}

function localInputToISO(value: string): string | undefined {
  if (!value) return undefined
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? undefined : date.toISOString()
}

function metadataNumber(metadata: Record<string, unknown> | null | undefined, key: string, fallback: number): number {
  const value = metadata?.[key]
  if (typeof value === 'number' && Number.isFinite(value)) return value
  if (typeof value === 'string') {
    const parsed = Number(value)
    if (Number.isFinite(parsed)) return parsed
  }
  return fallback
}

function rewardLabel(prize: LotteryPrize): string {
  if (prize.redeem_type === 'balance' || prize.redeem_type === 'timed_quota') {
    const suffix = prize.redeem_type === 'timed_quota' ? ` · ${prize.redeem_validity_days}${tx('天', 'd')}` : ''
    return `${formatCurrency(prize.redeem_value)}${suffix}`
  }
  if (prize.redeem_type === 'random_timed_quota') {
    const min = metadataNumber(prize.redeem_metadata, 'min_value', 0)
    const max = metadataNumber(prize.redeem_metadata, 'max_value', 0)
    return `${formatCurrency(min)} - ${formatCurrency(max)} · ${prize.redeem_validity_days}${tx('天', 'd')}`
  }
  if (prize.redeem_type === 'subscription') return `${tx('订阅', 'Subscription')} · ${prize.redeem_validity_days}${tx('天', 'd')}`
  if (prize.redeem_type === 'concurrency') return `${prize.redeem_value} ${tx('并发', 'concurrency')}`
  return tx('邀请码', 'Invitation')
}

async function loadCampaigns(page = pagination.page): Promise<void> {
  loading.value = true
  try {
    const result = await lotteryAPI.listCampaigns({ page, page_size: pagination.page_size, status: statusFilter.value })
    campaigns.value = result.items || []
    pagination.page = result.page
    pagination.page_size = result.page_size
    pagination.total = result.total
    pagination.pages = result.pages
    if (!selectedCampaignId.value && campaigns.value.length > 0) {
      await selectCampaign(campaigns.value[0].id)
    } else if (selectedCampaignId.value && !campaigns.value.some((item) => item.id === selectedCampaignId.value)) {
      selectedCampaignId.value = campaigns.value[0]?.id ?? null
      prizes.value = []
      if (selectedCampaignId.value) await loadPrizes(selectedCampaignId.value)
    }
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tx('加载抽奖活动失败', 'Failed to load campaigns')))
  } finally {
    loading.value = false
  }
}

async function changePage(page: number): Promise<void> {
  if (page < 1 || page > pagination.pages) return
  await loadCampaigns(page)
}

async function selectCampaign(id: number): Promise<void> {
  selectedCampaignId.value = id
  resetPrizeForm()
  await loadPrizes(id)
}

async function loadPrizes(campaignId: number): Promise<void> {
  prizesLoading.value = true
  try {
    prizes.value = await lotteryAPI.listPrizes(campaignId)
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tx('加载奖品失败', 'Failed to load prizes')))
  } finally {
    prizesLoading.value = false
  }
}

function resetCampaignForm(): void {
  campaignForm.id = null
  campaignForm.name = ''
  campaignForm.description = ''
  campaignForm.status = 'draft'
  campaignForm.starts_at = ''
  campaignForm.ends_at = ''
  campaignForm.chance_expires_in_days = 30
}

function editCampaign(campaign: LotteryCampaign): void {
  campaignForm.id = campaign.id
  campaignForm.name = campaign.name
  campaignForm.description = campaign.description || ''
  campaignForm.status = campaign.status
  campaignForm.starts_at = toLocalInput(campaign.starts_at)
  campaignForm.ends_at = toLocalInput(campaign.ends_at)
  campaignForm.chance_expires_in_days = campaign.chance_expires_in_days || 30
}

async function saveCampaign(): Promise<void> {
  if (!campaignForm.name.trim()) return
  campaignSaving.value = true
  try {
    const startsAt = localInputToISO(campaignForm.starts_at)
    const endsAt = localInputToISO(campaignForm.ends_at)
    if (campaignForm.id) {
      const payload: UpdateLotteryCampaignRequest = {
        name: campaignForm.name,
        description: campaignForm.description,
        status: campaignForm.status,
        chance_expires_in_days: Number(campaignForm.chance_expires_in_days) || 30,
        clear_ends_at: !endsAt,
      }
      if (startsAt) payload.starts_at = startsAt
      if (endsAt) payload.ends_at = endsAt
      await lotteryAPI.updateCampaign(campaignForm.id, payload)
      appStore.showSuccess(tx('活动已保存', 'Campaign saved'))
    } else {
      const payload: CreateLotteryCampaignRequest = {
        name: campaignForm.name,
        description: campaignForm.description,
        status: campaignForm.status,
        chance_expires_in_days: Number(campaignForm.chance_expires_in_days) || 30,
      }
      if (startsAt) payload.starts_at = startsAt
      if (endsAt) payload.ends_at = endsAt
      const created = await lotteryAPI.createCampaign(payload)
      selectedCampaignId.value = created.id
      appStore.showSuccess(tx('活动已创建', 'Campaign created'))
    }
    resetCampaignForm()
    await loadCampaigns()
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tx('保存活动失败', 'Failed to save campaign')))
  } finally {
    campaignSaving.value = false
  }
}

function resetPrizeForm(): void {
  prizeForm.id = null
  prizeForm.name = ''
  prizeForm.description = ''
  prizeForm.status = 'active'
  prizeForm.weight = 1
  prizeForm.stock_total = 1
  prizeForm.redeem_type = 'balance'
  prizeForm.redeem_value = 0
  prizeForm.redeem_group_id = ''
  prizeForm.redeem_validity_days = 30
  prizeForm.min_value = 1
  prizeForm.max_value = 10
  prizeForm.sort_order = 0
}

function editPrize(prize: LotteryPrize): void {
  prizeForm.id = prize.id
  prizeForm.name = prize.name
  prizeForm.description = prize.description || ''
  prizeForm.status = prize.status
  prizeForm.weight = prize.weight
  prizeForm.stock_total = prize.stock_total
  prizeForm.redeem_type = prize.redeem_type
  prizeForm.redeem_value = prize.redeem_value
  prizeForm.redeem_group_id = prize.redeem_group_id ? String(prize.redeem_group_id) : ''
  prizeForm.redeem_validity_days = prize.redeem_validity_days || 30
  prizeForm.min_value = metadataNumber(prize.redeem_metadata, 'min_value', 1)
  prizeForm.max_value = metadataNumber(prize.redeem_metadata, 'max_value', 10)
  prizeForm.sort_order = prize.sort_order
}

function validatePrizeForm(): boolean {
  const weight = Number(prizeForm.weight)
  const stockTotal = Number(prizeForm.stock_total)
  const validityDays = Number(prizeForm.redeem_validity_days)
  if (!Number.isFinite(weight) || weight < 0 || !Number.isFinite(stockTotal) || stockTotal < 0) {
    appStore.showError(tx('权重和库存必须是非负数', 'Weight and stock must be non-negative numbers'))
    return false
  }
  if (['subscription', 'timed_quota', 'random_timed_quota'].includes(prizeForm.redeem_type) && (!Number.isFinite(validityDays) || validityDays < 1)) {
    appStore.showError(tx('有效天数必须大于 0', 'Validity days must be greater than 0'))
    return false
  }
  if (prizeForm.redeem_type === 'subscription') {
    const groupID = Number(prizeForm.redeem_group_id)
    if (!Number.isFinite(groupID) || groupID <= 0) {
      appStore.showError(tx('订阅奖品必须填写有效分组 ID', 'Subscription prizes require a valid group ID'))
      return false
    }
  }
  if (prizeForm.redeem_type === 'balance' || prizeForm.redeem_type === 'timed_quota') {
    const value = Number(prizeForm.redeem_value)
    if (!Number.isFinite(value) || value <= 0) {
      appStore.showError(tx('奖励值必须大于 0', 'Reward value must be greater than 0'))
      return false
    }
  }
  if (prizeForm.redeem_type === 'random_timed_quota') {
    const min = Number(prizeForm.min_value)
    const max = Number(prizeForm.max_value)
    if (!Number.isFinite(min) || !Number.isFinite(max) || min <= 0 || max < min) {
      appStore.showError(tx('随机限时额度范围不合法', 'Random timed quota range is invalid'))
      return false
    }
  }
  return true
}

function buildPrizePayload(): CreateLotteryPrizeRequest {
  const payload: CreateLotteryPrizeRequest = {
    name: prizeForm.name,
    description: prizeForm.description,
    status: prizeForm.status,
    weight: Number(prizeForm.weight) || 0,
    stock_total: Number(prizeForm.stock_total) || 0,
    redeem_type: prizeForm.redeem_type,
    redeem_value: prizeForm.redeem_type === 'random_timed_quota' || prizeForm.redeem_type === 'invitation' ? 0 : Number(prizeForm.redeem_value) || 0,
    redeem_validity_days: Number(prizeForm.redeem_validity_days) || 30,
    sort_order: Number(prizeForm.sort_order) || 0,
  }
  if (prizeForm.redeem_type === 'subscription' && prizeForm.redeem_group_id) {
    payload.redeem_group_id = Number(prizeForm.redeem_group_id)
  }
  if (prizeForm.redeem_type === 'random_timed_quota') {
    payload.redeem_metadata = {
      min_value: Number(prizeForm.min_value) || 0,
      max_value: Number(prizeForm.max_value) || 0,
    }
  }
  return payload
}

async function savePrize(): Promise<void> {
  if (!selectedCampaign.value || !prizeForm.name.trim()) return
  if (!validatePrizeForm()) return
  prizeSaving.value = true
  try {
    const payload = buildPrizePayload()
    if (prizeForm.id) {
      const updatePayload: UpdateLotteryPrizeRequest = { ...payload }
      if (prizeForm.redeem_type !== 'subscription' || !prizeForm.redeem_group_id) {
        updatePayload.clear_redeem_group_id = true
      }
      await lotteryAPI.updatePrize(prizeForm.id, updatePayload)
      appStore.showSuccess(tx('奖品已保存', 'Prize saved'))
    } else {
      await lotteryAPI.createPrize(selectedCampaign.value.id, payload)
      appStore.showSuccess(tx('奖品已添加', 'Prize added'))
    }
    resetPrizeForm()
    await loadPrizes(selectedCampaign.value.id)
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tx('保存奖品失败', 'Failed to save prize')))
  } finally {
    prizeSaving.value = false
  }
}

async function grantChances(): Promise<void> {
  if (!selectedCampaign.value || !grantForm.user_id) return
  granting.value = true
  try {
    const expiresAt = localInputToISO(grantForm.expires_at)
    const granted = await lotteryAPI.grantChances(selectedCampaign.value.id, {
      user_id: Number(grantForm.user_id),
      count: Number(grantForm.count) || 1,
      source: grantForm.source || 'admin',
      source_id: grantForm.source_id,
      expires_at: expiresAt || undefined,
    })
    appStore.showSuccess(tx(`已发放 ${granted.length} 次抽奖机会`, `Granted ${granted.length} chances`))
    grantForm.user_id = ''
    grantForm.count = 1
    grantForm.source_id = ''
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tx('发放抽奖机会失败', 'Failed to grant chances')))
  } finally {
    granting.value = false
  }
}

onMounted(() => {
  loadCampaigns()
})
</script>
