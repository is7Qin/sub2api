<template>
  <AppLayout>
    <div class="space-y-6">
      <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div>
          <h1 class="text-2xl font-bold text-gray-900 dark:text-white">{{ t('admin.rechargeReset.title') }}</h1>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.rechargeReset.description') }}</p>
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
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ tx('规则数', 'Rules') }}</p>
          <p class="mt-2 text-2xl font-semibold text-primary-600 dark:text-primary-400">{{ totalRuleCount }}</p>
        </div>
        <div class="card p-5">
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ tx('当前活动规则', 'Selected Rules') }}</p>
          <p class="mt-2 text-2xl font-semibold text-amber-600 dark:text-amber-400">{{ rules.length }}</p>
        </div>
      </div>

      <div class="grid gap-6 xl:grid-cols-[minmax(0,1.15fr)_minmax(380px,0.85fr)]">
        <div class="card overflow-hidden">
          <div class="flex flex-col gap-3 border-b border-gray-100 px-6 py-4 dark:border-dark-700 sm:flex-row sm:items-center sm:justify-between">
            <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ tx('重置活动', 'Reset Campaigns') }}</h2>
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
            {{ tx('暂无充值重置活动', 'No recharge reset campaigns yet.') }}
          </div>
          <div v-else class="overflow-x-auto">
            <table class="w-full min-w-[840px] text-left text-sm">
              <thead>
                <tr class="border-b border-gray-100 text-gray-500 dark:border-dark-700 dark:text-gray-400">
                  <th class="px-6 py-3 font-medium">{{ tx('名称', 'Name') }}</th>
                  <th class="px-4 py-3 font-medium">{{ tx('状态', 'Status') }}</th>
                  <th class="px-4 py-3 font-medium">{{ tx('重置周期', 'Reset Windows') }}</th>
                  <th class="px-4 py-3 font-medium">{{ tx('开始时间', 'Starts At') }}</th>
                  <th class="px-4 py-3 font-medium">{{ tx('结束时间', 'Ends At') }}</th>
                  <th class="px-4 py-3 font-medium">{{ tx('规则数', 'Rules') }}</th>
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
                    <span class="rounded-full px-2 py-0.5 text-xs font-medium" :class="campaignStatusClass(campaign.status)">{{ statusLabel(campaign.status) }}</span>
                  </td>
                  <td class="px-4 py-4 text-gray-700 dark:text-gray-300">{{ resetWindowLabel(campaign) }}</td>
                  <td class="px-4 py-4 text-gray-700 dark:text-gray-300">{{ formatDateTime(campaign.starts_at) }}</td>
                  <td class="px-4 py-4 text-gray-700 dark:text-gray-300">{{ formatDateTime(campaign.ends_at) || tx('不限', 'No limit') }}</td>
                  <td class="px-4 py-4 text-gray-700 dark:text-gray-300">{{ campaign.rules?.length || 0 }}</td>
                  <td class="px-6 py-4">
                    <div class="flex justify-end gap-2">
                      <button class="btn btn-sm btn-secondary" @click="startEditCampaign(campaign)">{{ tx('编辑', 'Edit') }}</button>
                      <button class="btn btn-sm btn-secondary" @click="selectCampaign(campaign)">{{ tx('规则', 'Rules') }}</button>
                    </div>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>

        <div class="card">
          <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
            <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ formMode === 'create' ? tx('新建活动', 'Create Campaign') : tx('编辑活动', 'Edit Campaign') }}</h2>
            <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ tx('至少选择一个要重置的订阅周期。', 'Select at least one subscription period to reset.') }}</p>
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
                <label class="label">{{ tx('开始时间', 'Starts At') }}</label>
                <input v-model="campaignForm.starts_at" class="input" required type="datetime-local" />
              </div>
            </div>
            <div>
              <label class="label">{{ tx('结束时间', 'Ends At') }}</label>
              <input v-model="campaignForm.ends_at" class="input" type="datetime-local" />
            </div>
            <div>
              <label class="label">{{ tx('重置周期', 'Reset Windows') }}</label>
              <div class="mt-2 grid gap-2 sm:grid-cols-3">
                <label class="flex items-center gap-2 rounded-lg border border-gray-200 px-3 py-2 text-sm dark:border-dark-700">
                  <input v-model="campaignForm.reset_daily" type="checkbox" />
                  <span>{{ tx('日额度', 'Daily') }}</span>
                </label>
                <label class="flex items-center gap-2 rounded-lg border border-gray-200 px-3 py-2 text-sm dark:border-dark-700">
                  <input v-model="campaignForm.reset_weekly" type="checkbox" />
                  <span>{{ tx('周额度', 'Weekly') }}</span>
                </label>
                <label class="flex items-center gap-2 rounded-lg border border-gray-200 px-3 py-2 text-sm dark:border-dark-700">
                  <input v-model="campaignForm.reset_monthly" type="checkbox" />
                  <span>{{ tx('月额度', 'Monthly') }}</span>
                </label>
              </div>
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

      <div v-if="selectedCampaign" class="grid gap-6 xl:grid-cols-[minmax(0,1fr)_420px]">
        <div class="card overflow-hidden">
          <div class="flex items-center justify-between border-b border-gray-100 px-6 py-4 dark:border-dark-700">
            <div>
              <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ tx('规则', 'Rules') }}</h2>
              <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ selectedCampaign.name }}</p>
            </div>
            <button class="btn btn-secondary" :disabled="loadingRules" @click="loadRules(selectedCampaign.id)">
              <Icon name="refresh" size="sm" :class="loadingRules ? 'animate-spin' : ''" />
              <span>{{ t('common.refresh') }}</span>
            </button>
          </div>
          <div v-if="rules.length === 0" class="p-8 text-center text-sm text-gray-500 dark:text-gray-400">
            {{ tx('暂无规则', 'No rules yet.') }}
          </div>
          <div v-else class="overflow-x-auto">
            <table class="w-full min-w-[620px] text-left text-sm">
              <thead>
                <tr class="border-b border-gray-100 text-gray-500 dark:border-dark-700 dark:text-gray-400">
                  <th class="px-6 py-3 font-medium">{{ tx('分组ID', 'Group ID') }}</th>
                  <th class="px-4 py-3 font-medium">{{ tx('充值阈值', 'Threshold') }}</th>
                  <th class="px-4 py-3 font-medium">{{ tx('状态', 'Status') }}</th>
                  <th class="px-4 py-3 font-medium">{{ tx('更新时间', 'Updated') }}</th>
                  <th class="px-6 py-3 text-right font-medium">{{ tx('操作', 'Actions') }}</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="rule in rules" :key="rule.id" class="border-b border-gray-100 last:border-b-0 dark:border-dark-800">
                  <td class="px-6 py-4 text-gray-900 dark:text-white">#{{ rule.group_id }}</td>
                  <td class="px-4 py-4 text-gray-700 dark:text-gray-300">{{ formatCurrency(rule.threshold_amount) }}</td>
                  <td class="px-4 py-4">
                    <span class="rounded-full px-2 py-0.5 text-xs font-medium" :class="ruleStatusClass(rule.status)">{{ ruleStatusLabel(rule.status) }}</span>
                  </td>
                  <td class="px-4 py-4 text-gray-700 dark:text-gray-300">{{ formatDateTime(rule.updated_at) }}</td>
                  <td class="px-6 py-4 text-right">
                    <button class="text-sm text-primary-600 hover:underline dark:text-primary-400" @click="startEditRule(rule)">{{ tx('编辑', 'Edit') }}</button>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>

        <div class="card">
          <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
            <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ ruleFormMode === 'create' ? tx('新建规则', 'Create Rule') : tx('编辑规则', 'Edit Rule') }}</h2>
            <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ tx('用户在该分组有有效订阅且余额充值达到阈值时触发重置。', 'Triggered when a balance recharge reaches the threshold for a user with an active subscription in this group.') }}</p>
          </div>
          <form class="space-y-4 p-6" @submit.prevent="saveRule">
            <div>
              <label class="label">{{ tx('分组ID', 'Group ID') }}</label>
              <input v-model.number="ruleForm.group_id" class="input" min="1" required type="number" />
            </div>
            <div>
              <label class="label">{{ tx('充值阈值', 'Threshold Amount') }}</label>
              <input v-model.number="ruleForm.threshold_amount" class="input" min="0.01" required step="0.01" type="number" />
            </div>
            <div>
              <label class="label">{{ tx('状态', 'Status') }}</label>
              <select v-model="ruleForm.status" class="input">
                <option value="active">{{ ruleStatusLabel('active') }}</option>
                <option value="disabled">{{ ruleStatusLabel('disabled') }}</option>
              </select>
            </div>
            <div class="flex justify-end gap-2">
              <button v-if="ruleFormMode === 'edit'" class="btn btn-secondary" type="button" @click="startCreateRule">{{ tx('取消编辑', 'Cancel Edit') }}</button>
              <button class="btn btn-primary" :disabled="savingRule" type="submit">
                <Icon v-if="savingRule" name="refresh" size="sm" class="animate-spin" />
                <span>{{ tx('保存', 'Save') }}</span>
              </button>
            </div>
          </form>
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
import rechargeResetAPI from '@/api/admin/rechargeReset'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatCurrency, formatDateTime, formatDateTimeLocalInput, parseDateTimeLocalInput } from '@/utils/format'
import type {
  CreateRechargeResetCampaignRequest,
  RechargeResetCampaign,
  RechargeResetCampaignRule,
  RechargeResetCampaignStatus,
  RechargeResetRuleStatus,
} from '@/api/admin/rechargeReset'

const { t, locale } = useI18n()
const appStore = useAppStore()

const loadingCampaigns = ref(false)
const loadingRules = ref(false)
const savingCampaign = ref(false)
const savingRule = ref(false)
const statusFilter = ref('')
const campaigns = ref<RechargeResetCampaign[]>([])
const selectedCampaign = ref<RechargeResetCampaign | null>(null)
const rules = ref<RechargeResetCampaignRule[]>([])
const formMode = ref<'create' | 'edit'>('create')
const editingCampaignId = ref<number | null>(null)
const ruleFormMode = ref<'create' | 'edit'>('create')
const editingRuleId = ref<number | null>(null)

const activeCampaignCount = computed(() => campaigns.value.filter((item) => item.status === 'active').length)
const totalRuleCount = computed(() => campaigns.value.reduce((sum, item) => sum + (item.rules?.length || 0), 0))

const campaignForm = reactive({
  name: '',
  description: '',
  status: 'draft' as RechargeResetCampaignStatus,
  starts_at: '',
  ends_at: '',
  reset_daily: true,
  reset_weekly: true,
  reset_monthly: true,
})

const ruleForm = reactive({
  group_id: 0,
  threshold_amount: 0,
  status: 'active' as RechargeResetRuleStatus,
})

function tx(zh: string, en: string): string {
  return String(locale.value).startsWith('zh') ? zh : en
}

function toLocalInput(value: string | null | undefined): string {
  if (!value) return ''
  const timestampSeconds = Math.floor(new Date(value).getTime() / 1000)
  return formatDateTimeLocalInput(Number.isNaN(timestampSeconds) ? null : timestampSeconds)
}

function localInputToISO(value: string): string | undefined {
  const timestampSeconds = parseDateTimeLocalInput(value)
  return timestampSeconds === null ? undefined : new Date(timestampSeconds * 1000).toISOString()
}

function defaultStartInput(): string {
  return formatDateTimeLocalInput(Math.floor(Date.now() / 1000))
}

function statusLabel(status: RechargeResetCampaignStatus): string {
  const labels: Record<RechargeResetCampaignStatus, [string, string]> = {
    draft: ['草稿', 'Draft'],
    active: ['启用', 'Active'],
    disabled: ['停用', 'Disabled'],
    ended: ['已结束', 'Ended'],
  }
  const [zh, en] = labels[status] || [status, status]
  return tx(zh, en)
}

function ruleStatusLabel(status: RechargeResetRuleStatus): string {
  return status === 'active' ? tx('启用', 'Active') : tx('停用', 'Disabled')
}

function campaignStatusClass(status: string): string {
  if (status === 'active') return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300'
  if (status === 'draft') return 'bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-300'
  if (status === 'disabled') return 'bg-gray-100 text-gray-700 dark:bg-dark-700 dark:text-gray-300'
  return 'bg-blue-100 text-blue-700 dark:bg-blue-900/40 dark:text-blue-300'
}

function ruleStatusClass(status: string): string {
  if (status === 'active') return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300'
  return 'bg-gray-100 text-gray-700 dark:bg-dark-700 dark:text-gray-300'
}

function resetWindowLabel(campaign: Pick<RechargeResetCampaign, 'reset_daily' | 'reset_weekly' | 'reset_monthly'>): string {
  const windows: string[] = []
  if (campaign.reset_daily) windows.push(tx('日', 'Daily'))
  if (campaign.reset_weekly) windows.push(tx('周', 'Weekly'))
  if (campaign.reset_monthly) windows.push(tx('月', 'Monthly'))
  return windows.join(' / ') || '-'
}

function resetCampaignForm(): void {
  Object.assign(campaignForm, {
    name: '',
    description: '',
    status: 'draft',
    starts_at: defaultStartInput(),
    ends_at: '',
    reset_daily: true,
    reset_weekly: true,
    reset_monthly: true,
  })
}

function resetRuleForm(): void {
  Object.assign(ruleForm, {
    group_id: 0,
    threshold_amount: 0,
    status: 'active',
  })
}

function startCreateCampaign(): void {
  formMode.value = 'create'
  editingCampaignId.value = null
  resetCampaignForm()
}

function startEditCampaign(campaign: RechargeResetCampaign): void {
  formMode.value = 'edit'
  editingCampaignId.value = campaign.id
  Object.assign(campaignForm, {
    name: campaign.name,
    description: campaign.description,
    status: campaign.status,
    starts_at: toLocalInput(campaign.starts_at),
    ends_at: toLocalInput(campaign.ends_at),
    reset_daily: campaign.reset_daily,
    reset_weekly: campaign.reset_weekly,
    reset_monthly: campaign.reset_monthly,
  })
}

function startCreateRule(): void {
  ruleFormMode.value = 'create'
  editingRuleId.value = null
  resetRuleForm()
}

function startEditRule(rule: RechargeResetCampaignRule): void {
  ruleFormMode.value = 'edit'
  editingRuleId.value = rule.id
  Object.assign(ruleForm, {
    group_id: rule.group_id,
    threshold_amount: rule.threshold_amount,
    status: rule.status,
  })
}

function buildCampaignPayload(): CreateRechargeResetCampaignRequest | null {
  const startsAt = localInputToISO(campaignForm.starts_at)
  const endsAt = campaignForm.ends_at ? localInputToISO(campaignForm.ends_at) : undefined
  if (!startsAt) {
    appStore.showError(tx('开始时间不合法', 'Invalid start time'))
    return null
  }
  if (campaignForm.ends_at && !endsAt) {
    appStore.showError(tx('结束时间不合法', 'Invalid end time'))
    return null
  }
  const payload: CreateRechargeResetCampaignRequest = {
    name: campaignForm.name,
    description: campaignForm.description,
    status: campaignForm.status,
    starts_at: startsAt,
    reset_daily: campaignForm.reset_daily,
    reset_weekly: campaignForm.reset_weekly,
    reset_monthly: campaignForm.reset_monthly,
  }
  if (endsAt) payload.ends_at = endsAt
  return payload
}

async function loadCampaigns(): Promise<void> {
  loadingCampaigns.value = true
  try {
    const result = await rechargeResetAPI.listCampaigns({ page: 1, page_size: 100, status: statusFilter.value || undefined })
    campaigns.value = result.items || []
    if (!selectedCampaign.value && campaigns.value.length > 0) {
      await selectCampaign(campaigns.value[0])
    } else if (selectedCampaign.value) {
      const updated = campaigns.value.find((item) => item.id === selectedCampaign.value?.id)
      if (updated) selectedCampaign.value = updated
    }
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tx('加载充值重置活动失败', 'Failed to load recharge reset campaigns')))
  } finally {
    loadingCampaigns.value = false
  }
}

async function saveCampaign(): Promise<void> {
  if (!campaignForm.name || !campaignForm.starts_at) return
  if (!campaignForm.reset_daily && !campaignForm.reset_weekly && !campaignForm.reset_monthly) {
    appStore.showError(tx('至少选择一个重置周期', 'Select at least one reset window'))
    return
  }
  savingCampaign.value = true
  try {
    const payload = buildCampaignPayload()
    if (!payload) return
    const wasCreating = formMode.value === 'create'
    const item = formMode.value === 'edit' && editingCampaignId.value
      ? await rechargeResetAPI.updateCampaign(editingCampaignId.value, {
        ...payload,
        ends_at: campaignForm.ends_at ? payload.ends_at : null,
      })
      : await rechargeResetAPI.createCampaign(payload)
    appStore.showSuccess(tx('充值重置活动已保存', 'Recharge reset campaign saved'))
    await loadCampaigns()
    await selectCampaign(item)
    if (wasCreating) resetCampaignForm()
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tx('保存充值重置活动失败', 'Failed to save recharge reset campaign')))
  } finally {
    savingCampaign.value = false
  }
}

async function selectCampaign(campaign: RechargeResetCampaign): Promise<void> {
  selectedCampaign.value = campaign
  startCreateRule()
  await loadRules(campaign.id)
}

async function loadRules(campaignId: number): Promise<void> {
  loadingRules.value = true
  try {
    rules.value = await rechargeResetAPI.listRules(campaignId)
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tx('加载规则失败', 'Failed to load rules')))
  } finally {
    loadingRules.value = false
  }
}

async function saveRule(): Promise<void> {
  if (!selectedCampaign.value) return
  const groupID = Number(ruleForm.group_id)
  const thresholdAmount = Number(ruleForm.threshold_amount)
  if (!Number.isFinite(groupID) || groupID <= 0) {
    appStore.showError(tx('分组 ID 必须大于 0', 'Group ID must be greater than 0'))
    return
  }
  if (!Number.isFinite(thresholdAmount) || thresholdAmount <= 0) {
    appStore.showError(tx('充值阈值必须大于 0', 'Threshold amount must be greater than 0'))
    return
  }
  savingRule.value = true
  try {
    const payload = {
      group_id: groupID,
      threshold_amount: thresholdAmount,
      status: ruleForm.status,
    }
    if (ruleFormMode.value === 'edit' && editingRuleId.value) {
      await rechargeResetAPI.updateRule(editingRuleId.value, payload)
    } else {
      await rechargeResetAPI.createRule(selectedCampaign.value.id, payload)
    }
    appStore.showSuccess(tx('规则已保存', 'Rule saved'))
    await loadRules(selectedCampaign.value.id)
    await loadCampaigns()
    startCreateRule()
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tx('保存规则失败', 'Failed to save rule')))
  } finally {
    savingRule.value = false
  }
}

onMounted(() => {
  resetCampaignForm()
  loadCampaigns()
})
</script>
