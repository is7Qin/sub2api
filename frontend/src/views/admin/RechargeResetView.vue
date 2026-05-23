<template>
  <AppLayout>
    <div class="space-y-6">
      <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div>
          <h1 class="text-2xl font-bold text-gray-900 dark:text-white">{{ t('admin.rechargeReset.title') }}</h1>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.rechargeReset.description') }}</p>
        </div>
        <button class="btn btn-primary" type="button" @click="openCreateCampaignDialog">
          <Icon name="plus" size="sm" />
          <span>{{ tx('新建活动', 'New Campaign') }}</span>
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
            :actions-count="2"
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
              <span class="rounded-full px-2 py-0.5 text-xs font-medium" :class="campaignStatusClass(row.status)">{{ statusLabel(row.status) }}</span>
            </template>

            <template #cell-reset_windows="{ row }">
              <span>{{ resetWindowLabel(row) }}</span>
            </template>

            <template #cell-starts_at="{ row }">
              <span>{{ formatDateTime(row.starts_at) }}</span>
            </template>

            <template #cell-ends_at="{ row }">
              <span>{{ formatDateTime(row.ends_at) || tx('不限', 'No limit') }}</span>
            </template>

            <template #cell-rules="{ row }">
              <span>{{ row.rules?.length || 0 }}</span>
            </template>

            <template #cell-actions="{ row }">
              <div class="flex justify-end gap-2">
                <button class="btn btn-sm btn-secondary" type="button" @click="openEditCampaignDialog(row)">{{ tx('编辑', 'Edit') }}</button>
                <button class="btn btn-sm btn-primary" type="button" @click="openRulesDialog(row)">{{ tx('规则', 'Rules') }}</button>
              </div>
            </template>

            <template #empty>
              <div class="flex flex-col items-center py-8 text-center">
                <p class="text-base font-medium text-gray-900 dark:text-white">{{ tx('暂无充值重置活动', 'No recharge reset campaigns yet.') }}</p>
                <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ tx('新建活动后，在规则弹窗里用分组选择器配置触发条件。', 'Create a campaign, then configure trigger rules with the group selector.') }}</p>
                <button class="btn btn-primary mt-4" type="button" @click="openCreateCampaignDialog">
                  <Icon name="plus" size="sm" />
                  <span>{{ tx('新建活动', 'New Campaign') }}</span>
                </button>
              </div>
            </template>
          </DataTable>
        </template>
      </TablePageLayout>
    </div>

    <BaseDialog
      :show="showCampaignDialog"
      :title="formMode === 'create' ? tx('新建活动', 'Create Campaign') : tx('编辑活动', 'Edit Campaign')"
      width="wide"
      @close="closeCampaignDialog"
    >
      <form id="recharge-reset-campaign-form" class="space-y-4" @submit.prevent="saveCampaign">
        <div>
          <label class="label">{{ tx('名称', 'Name') }}</label>
          <input v-model.trim="campaignForm.name" class="input" required />
        </div>
        <div>
          <label class="label">{{ tx('说明', 'Description') }}</label>
          <textarea v-model="campaignForm.description" class="input min-h-[80px]"></textarea>
        </div>
        <div class="grid gap-4 sm:grid-cols-2">
          <div>
            <label class="label">{{ tx('状态', 'Status') }}</label>
            <Select v-model="campaignForm.status" :options="campaignStatusOptions" />
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
          <p class="mt-2 text-xs text-gray-500 dark:text-gray-400">{{ tx('至少选择一个要重置的订阅周期。', 'Select at least one subscription period to reset.') }}</p>
        </div>
      </form>
      <template #footer>
        <button class="btn btn-secondary" type="button" @click="closeCampaignDialog">{{ tx('取消', 'Cancel') }}</button>
        <button class="btn btn-primary" :disabled="savingCampaign" form="recharge-reset-campaign-form" type="submit">
          <Icon v-if="savingCampaign" name="refresh" size="sm" class="animate-spin" />
          <span>{{ tx('保存', 'Save') }}</span>
        </button>
      </template>
    </BaseDialog>

    <BaseDialog
      :show="showRulesDialog"
      :title="activeCampaign ? `${tx('规则', 'Rules')} · ${activeCampaign.name}` : tx('规则', 'Rules')"
      width="extra-wide"
      @close="closeRulesDialog"
    >
      <div v-if="activeCampaign" class="space-y-5">
        <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
          <div class="text-sm text-gray-500 dark:text-gray-400">
            <span>{{ tx('活动', 'Campaign') }} #{{ activeCampaign.id }}</span>
            <span class="mx-2">·</span>
            <span>{{ resetWindowLabel(activeCampaign) }}</span>
          </div>
          <button class="btn btn-secondary btn-sm" :disabled="loadingRules" type="button" @click="loadRules(activeCampaign.id)">
            <Icon name="refresh" size="sm" :class="loadingRules ? 'animate-spin' : ''" />
            <span>{{ t('common.refresh') }}</span>
          </button>
        </div>

        <form class="grid gap-4 rounded-lg border border-gray-200 p-4 dark:border-dark-700 lg:grid-cols-[minmax(240px,1fr)_180px_150px_auto]" @submit.prevent="saveRule">
          <div>
            <label class="label">{{ tx('分组', 'Group') }}</label>
            <Select
              v-model="ruleForm.group_id"
              :options="groupOptions"
              searchable
              :placeholder="tx('选择分组', 'Select group')"
            />
          </div>
          <div>
            <label class="label">{{ tx('充值阈值', 'Threshold Amount') }}</label>
            <input v-model.number="ruleForm.threshold_amount" class="input" min="0.01" required step="0.01" type="number" />
          </div>
          <div>
            <label class="label">{{ tx('状态', 'Status') }}</label>
            <Select v-model="ruleForm.status" :options="ruleStatusOptions" />
          </div>
          <div class="flex items-end gap-2">
            <button v-if="ruleFormMode === 'edit'" class="btn btn-secondary" type="button" @click="startCreateRule">{{ tx('取消', 'Cancel') }}</button>
            <button class="btn btn-primary" :disabled="savingRule" type="submit">
              <Icon v-if="savingRule" name="refresh" size="sm" class="animate-spin" />
              <span>{{ ruleFormMode === 'create' ? tx('添加', 'Add') : tx('保存', 'Save') }}</span>
            </button>
          </div>
        </form>

        <DataTable :columns="ruleColumns" :data="rules" :loading="loadingRules" row-key="id" :actions-count="1">
          <template #cell-group_id="{ row }">
            <div>
              <div class="font-medium text-gray-900 dark:text-white">{{ groupLabel(row.group_id) }}</div>
              <div class="text-xs text-gray-500 dark:text-gray-400">#{{ row.group_id }}</div>
            </div>
          </template>
          <template #cell-threshold_amount="{ row }">
            <span>{{ formatCurrency(row.threshold_amount) }}</span>
          </template>
          <template #cell-status="{ row }">
            <span class="rounded-full px-2 py-0.5 text-xs font-medium" :class="ruleStatusClass(row.status)">{{ ruleStatusLabel(row.status) }}</span>
          </template>
          <template #cell-updated_at="{ row }">
            <span>{{ formatDateTime(row.updated_at) }}</span>
          </template>
          <template #cell-actions="{ row }">
            <div class="flex justify-end">
              <button class="text-sm text-primary-600 hover:underline dark:text-primary-400" type="button" @click="startEditRule(row)">{{ tx('编辑', 'Edit') }}</button>
            </div>
          </template>
          <template #empty>
            <div class="py-8 text-center text-sm text-gray-500 dark:text-gray-400">{{ tx('暂无规则', 'No rules yet.') }}</div>
          </template>
        </DataTable>
      </div>
    </BaseDialog>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import DataTable from '@/components/common/DataTable.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Select, { type SelectOption } from '@/components/common/Select.vue'
import Icon from '@/components/icons/Icon.vue'
import { adminAPI } from '@/api/admin'
import rechargeResetAPI from '@/api/admin/rechargeReset'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatCurrency, formatDateTime, formatDateTimeLocalInput, parseDateTimeLocalInput } from '@/utils/format'
import type { AdminGroup } from '@/types'
import type { Column } from '@/components/common/types'
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
const statusFilter = ref<string | null>('')
const campaigns = ref<RechargeResetCampaign[]>([])
const groups = ref<AdminGroup[]>([])
const activeCampaign = ref<RechargeResetCampaign | null>(null)
const rules = ref<RechargeResetCampaignRule[]>([])
const formMode = ref<'create' | 'edit'>('create')
const editingCampaignId = ref<number | null>(null)
const ruleFormMode = ref<'create' | 'edit'>('create')
const editingRuleId = ref<number | null>(null)
const showCampaignDialog = ref(false)
const showRulesDialog = ref(false)

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
  group_id: null as number | null,
  threshold_amount: 0,
  status: 'active' as RechargeResetRuleStatus,
})

const campaignColumns = computed<Column[]>(() => [
  { key: 'name', label: tx('名称', 'Name'), sortable: true },
  { key: 'status', label: tx('状态', 'Status'), sortable: true },
  { key: 'reset_windows', label: tx('重置周期', 'Reset Windows') },
  { key: 'starts_at', label: tx('开始时间', 'Starts At') },
  { key: 'ends_at', label: tx('结束时间', 'Ends At') },
  { key: 'rules', label: tx('规则数', 'Rules') },
  { key: 'actions', label: tx('操作', 'Actions'), class: 'text-right' },
])

const ruleColumns = computed<Column[]>(() => [
  { key: 'group_id', label: tx('分组', 'Group') },
  { key: 'threshold_amount', label: tx('充值阈值', 'Threshold') },
  { key: 'status', label: tx('状态', 'Status') },
  { key: 'updated_at', label: tx('更新时间', 'Updated') },
  { key: 'actions', label: tx('操作', 'Actions'), class: 'text-right' },
])

const statusFilterOptions = computed<SelectOption[]>(() => [
  { value: '', label: tx('全部状态', 'All Status') },
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

const ruleStatusOptions = computed<SelectOption[]>(() => [
  { value: 'active', label: ruleStatusLabel('active') },
  { value: 'disabled', label: ruleStatusLabel('disabled') },
])

const groupOptions = computed<SelectOption[]>(() =>
  groups.value.map((group) => ({
    value: group.id,
    label: `${group.name} (#${group.id})`,
    description: [group.platform, group.status, group.subscription_type].filter(Boolean).join(' · '),
  }))
)

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

function groupLabel(groupID: number): string {
  const group = groups.value.find((item) => item.id === groupID)
  return group ? group.name : `#${groupID}`
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
    group_id: null,
    threshold_amount: 0,
    status: 'active',
  })
}

function openCreateCampaignDialog(): void {
  formMode.value = 'create'
  editingCampaignId.value = null
  resetCampaignForm()
  showCampaignDialog.value = true
}

function openEditCampaignDialog(campaign: RechargeResetCampaign): void {
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
  showCampaignDialog.value = true
}

function closeCampaignDialog(): void {
  showCampaignDialog.value = false
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

async function loadGroups(): Promise<void> {
  try {
    groups.value = await adminAPI.groups.getAll()
  } catch (error) {
    console.error('Failed to load groups:', error)
  }
}

async function loadCampaigns(): Promise<void> {
  loadingCampaigns.value = true
  try {
    const result = await rechargeResetAPI.listCampaigns({ page: 1, page_size: 100, status: statusFilter.value || undefined })
    campaigns.value = result.items || []
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
    const item = formMode.value === 'edit' && editingCampaignId.value
      ? await rechargeResetAPI.updateCampaign(editingCampaignId.value, {
        ...payload,
        ends_at: campaignForm.ends_at ? payload.ends_at : null,
      })
      : await rechargeResetAPI.createCampaign(payload)
    appStore.showSuccess(tx('充值重置活动已保存', 'Recharge reset campaign saved'))
    showCampaignDialog.value = false
    activeCampaign.value = item
    await loadCampaigns()
    if (showRulesDialog.value) await loadRules(item.id)
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tx('保存充值重置活动失败', 'Failed to save recharge reset campaign')))
  } finally {
    savingCampaign.value = false
  }
}

async function openRulesDialog(campaign: RechargeResetCampaign): Promise<void> {
  activeCampaign.value = campaign
  startCreateRule()
  showRulesDialog.value = true
  await loadRules(campaign.id)
}

function closeRulesDialog(): void {
  showRulesDialog.value = false
  startCreateRule()
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
  if (!activeCampaign.value) return
  const groupID = Number(ruleForm.group_id)
  const thresholdAmount = Number(ruleForm.threshold_amount)
  if (!Number.isFinite(groupID) || groupID <= 0) {
    appStore.showError(tx('请选择分组', 'Please select a group'))
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
      await rechargeResetAPI.createRule(activeCampaign.value.id, payload)
    }
    appStore.showSuccess(tx('规则已保存', 'Rule saved'))
    await loadRules(activeCampaign.value.id)
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
  loadGroups()
  loadCampaigns()
})
</script>
