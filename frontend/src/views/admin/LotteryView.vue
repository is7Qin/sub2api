<template>
  <AppLayout>
    <div class="space-y-6">
      <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div>
          <h1 class="text-2xl font-bold text-gray-900 dark:text-white">{{ tr('title') }}</h1>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
            {{ tr('description') }}
          </p>
        </div>
        <button class="btn btn-primary" type="button" @click="openCreateCampaignDialog">
          <Icon name="plus" size="sm" />
          <span>{{ tr('newCampaign') }}</span>
        </button>
      </div>

      <TablePageLayout>
        <template #filters>
          <div class="flex flex-wrap items-center gap-3">
            <div class="w-full sm:w-48">
              <Select v-model="statusFilter" :options="statusFilterOptions" @change="handleStatusFilterChange" />
            </div>
            <button class="btn btn-secondary" :disabled="loading" type="button" @click="loadCampaigns()">
              <Icon name="refresh" size="sm" :class="loading ? 'animate-spin' : ''" />
              <span>{{ t('common.refresh') }}</span>
            </button>
          </div>
        </template>

        <template #table>
          <DataTable
            :columns="campaignColumns"
            :data="campaigns"
            :loading="loading"
            row-key="id"
            sticky-actions-column
            :actions-count="3"
          >
            <template #cell-name="{ row }">
              <div class="min-w-0">
                <div class="flex flex-wrap items-center gap-2">
                  <span class="font-medium text-gray-900 dark:text-white">{{ row.name }}</span>
                  <span class="text-xs text-gray-400 dark:text-gray-500">#{{ row.id }}</span>
                </div>
                <p class="mt-1 max-w-xl truncate text-xs text-gray-500 dark:text-gray-400">
                  {{ row.description || tr('noDescription') }}
                </p>
              </div>
            </template>

            <template #cell-status="{ row }">
              <span class="rounded-full px-2 py-0.5 text-xs font-medium" :class="statusClass(row.status)">
                {{ statusLabel(row.status) }}
              </span>
            </template>

            <template #cell-window="{ row }">
              <div class="space-y-1 text-xs text-gray-600 dark:text-gray-300">
                <div>{{ tr('fields.start') }}: {{ formatDateTime(row.starts_at) || '-' }}</div>
                <div>{{ tr('fields.end') }}: {{ formatDateTime(row.ends_at) || tr('noLimit') }}</div>
              </div>
            </template>

            <template #cell-chance_expires_in_days="{ row }">
              <span>{{ row.chance_expires_in_days }} {{ tr('units.days') }}</span>
            </template>

            <template #cell-actions="{ row }">
              <div class="flex justify-end gap-2">
                <button class="btn btn-secondary btn-sm" type="button" @click="openPrizeManager(row)">
                  {{ tr('actions.prizes') }}
                </button>
                <button class="btn btn-secondary btn-sm" type="button" :disabled="row.status !== 'active'" @click="openGrantDialog(row)">
                  {{ tr('actions.grant') }}
                </button>
                <button class="btn btn-secondary btn-sm" type="button" @click="openEditCampaignDialog(row)">
                  <Icon name="edit" size="sm" />
                  <span>{{ tr('actions.edit') }}</span>
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

        <template #pagination>
          <Pagination
            v-if="pagination.total > 0"
            :page="pagination.page"
            :total="pagination.total"
            :page-size="pagination.page_size"
            @update:page="handlePageChange"
            @update:pageSize="handlePageSizeChange"
          />
        </template>
      </TablePageLayout>
    </div>

    <BaseDialog
      :show="showCampaignDialog"
      :title="campaignForm.id ? tr('dialogs.editCampaign') : tr('newCampaign')"
      width="wide"
      @close="closeCampaignDialog"
    >
      <form id="lottery-campaign-form" class="space-y-4" @submit.prevent="saveCampaign">
        <div>
          <label class="input-label">{{ tr('fields.name') }}</label>
          <input v-model.trim="campaignForm.name" class="input" required />
        </div>
        <div>
          <label class="input-label">{{ tr('fields.description') }}</label>
          <textarea v-model.trim="campaignForm.description" class="input min-h-[90px]"></textarea>
        </div>
        <div class="grid gap-4 md:grid-cols-2">
          <div>
            <label class="input-label">{{ tr('fields.status') }}</label>
            <Select v-model="campaignForm.status" :options="campaignStatusOptions" />
          </div>
          <div>
            <label class="input-label">{{ tr('fields.chanceTtlDays') }}</label>
            <input v-model.number="campaignForm.chance_expires_in_days" type="number" min="1" max="3650" class="input" />
          </div>
        </div>
        <div class="grid gap-4 md:grid-cols-2">
          <div>
            <label class="input-label">{{ tr('fields.startsAt') }}</label>
            <input v-model="campaignForm.starts_at" type="datetime-local" class="input" />
          </div>
          <div>
            <label class="input-label">{{ tr('fields.endsAt') }}</label>
            <input v-model="campaignForm.ends_at" type="datetime-local" class="input" />
          </div>
        </div>
      </form>
      <template #footer>
        <button class="btn btn-secondary" type="button" @click="closeCampaignDialog">{{ t('common.cancel') }}</button>
        <button class="btn btn-primary" :disabled="campaignSaving" form="lottery-campaign-form" type="submit">
          <Icon v-if="campaignSaving" name="refresh" size="sm" class="animate-spin" />
          <span>{{ campaignForm.id ? tr('actions.saveCampaign') : tr('actions.createCampaign') }}</span>
        </button>
      </template>
    </BaseDialog>

    <BaseDialog
      :show="showPrizeManagerDialog"
      :title="activeCampaign ? `${activeCampaign.name} · ${tr('dialogs.prizes')}` : tr('dialogs.prizes')"
      width="extra-wide"
      @close="closePrizeManager"
    >
      <div v-if="activeCampaign" class="space-y-4">
        <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
          <div class="text-sm text-gray-500 dark:text-gray-400">
            <span>{{ tr('fields.campaign') }} #{{ activeCampaign.id }}</span>
            <span class="mx-2">·</span>
            <span>{{ tr('fields.chanceTtl') }} {{ activeCampaign.chance_expires_in_days }} {{ tr('units.days') }}</span>
          </div>
          <div class="flex gap-2">
            <button class="btn btn-secondary btn-sm" :disabled="prizesLoading" type="button" @click="loadPrizes(activeCampaign.id)">
              <Icon name="refresh" size="sm" :class="prizesLoading ? 'animate-spin' : ''" />
              <span>{{ t('common.refresh') }}</span>
            </button>
            <button class="btn btn-primary btn-sm" type="button" @click="openCreatePrizeDialog">
              <Icon name="plus" size="sm" />
              <span>{{ tr('actions.addPrize') }}</span>
            </button>
          </div>
        </div>

        <DataTable :columns="prizeColumns" :data="prizes" :loading="prizesLoading" row-key="id" :actions-count="1">
          <template #cell-name="{ row }">
            <div>
              <div class="font-medium text-gray-900 dark:text-white">{{ row.name }}</div>
              <div class="text-xs text-gray-500 dark:text-gray-400">{{ row.description || '-' }}</div>
            </div>
          </template>

          <template #cell-status="{ row }">
            <span class="rounded-full px-2 py-0.5 text-xs font-medium" :class="prizeStatusClass(row.status)">
              {{ prizeStatusLabel(row.status) }}
            </span>
          </template>

          <template #cell-stock="{ row }">
            <span>{{ row.stock_used }} / {{ row.stock_total }}</span>
          </template>

          <template #cell-reward="{ row }">
            <span>{{ rewardLabel(row) }}</span>
          </template>

          <template #cell-actions="{ row }">
            <div class="flex justify-end">
              <button class="btn btn-secondary btn-sm" type="button" @click="openEditPrizeDialog(row)">
                {{ tr('actions.edit') }}
              </button>
            </div>
          </template>

          <template #empty>
            <div class="py-8 text-center text-sm text-gray-500 dark:text-gray-400">
              {{ tr('empty.noPrizes') }}
            </div>
          </template>
        </DataTable>
      </div>
    </BaseDialog>

    <BaseDialog
      :show="showPrizeDialog"
      :title="prizeForm.id ? tr('dialogs.editPrize') : tr('actions.addPrize')"
      width="wide"
      :z-index="60"
      @close="closePrizeDialog"
    >
      <form id="lottery-prize-form" class="space-y-4" @submit.prevent="savePrize">
        <div class="grid gap-4 md:grid-cols-2">
          <div>
            <label class="input-label">{{ tr('fields.prizeName') }}</label>
            <input v-model.trim="prizeForm.name" class="input" required />
          </div>
          <div>
            <label class="input-label">{{ tr('fields.status') }}</label>
            <Select v-model="prizeForm.status" :options="prizeStatusOptions" />
          </div>
        </div>
        <div>
          <label class="input-label">{{ tr('fields.description') }}</label>
          <textarea v-model.trim="prizeForm.description" class="input min-h-[80px]"></textarea>
        </div>
        <div class="grid gap-4 md:grid-cols-2">
          <div>
            <label class="input-label">{{ tr('fields.rewardType') }}</label>
            <Select v-model="prizeForm.redeem_type" :options="rewardTypeOptions" />
          </div>
          <div v-if="prizeForm.redeem_type === 'subscription'">
            <label class="input-label">{{ tr('fields.subscriptionGroup') }}</label>
            <Select
              v-model="prizeForm.redeem_group_id"
              :options="subscriptionGroupOptions"
              searchable
              :placeholder="tr('placeholders.selectGroup')"
            />
          </div>
        </div>
        <div class="grid gap-4 md:grid-cols-2">
          <div>
            <label class="input-label">{{ tr('fields.weight') }}</label>
            <input v-model.number="prizeForm.weight" type="number" min="0" class="input" />
          </div>
          <div>
            <label class="input-label">{{ tr('fields.totalStock') }}</label>
            <input v-model.number="prizeForm.stock_total" type="number" min="0" class="input" />
          </div>
        </div>
        <div v-if="prizeForm.redeem_type !== 'invitation' && prizeForm.redeem_type !== 'random_timed_quota'">
          <label class="input-label">{{ tr('fields.rewardValue') }}</label>
          <input v-model.number="prizeForm.redeem_value" type="number" min="0" step="0.01" class="input" />
        </div>
        <div v-if="prizeForm.redeem_type === 'random_timed_quota'" class="grid gap-4 md:grid-cols-2">
          <div>
            <label class="input-label">{{ tr('fields.minValue') }}</label>
            <input v-model.number="prizeForm.min_value" type="number" min="0" step="0.01" class="input" />
          </div>
          <div>
            <label class="input-label">{{ tr('fields.maxValue') }}</label>
            <input v-model.number="prizeForm.max_value" type="number" min="0" step="0.01" class="input" />
          </div>
        </div>
        <div v-if="['subscription', 'timed_quota', 'random_timed_quota'].includes(prizeForm.redeem_type)">
          <label class="input-label">{{ tr('fields.validityDays') }}</label>
          <input v-model.number="prizeForm.redeem_validity_days" type="number" min="1" max="3650" class="input" />
        </div>
        <div>
          <label class="input-label">{{ tr('fields.sortOrder') }}</label>
          <input v-model.number="prizeForm.sort_order" type="number" class="input" />
        </div>
      </form>
      <template #footer>
        <button class="btn btn-secondary" type="button" @click="closePrizeDialog">{{ t('common.cancel') }}</button>
        <button class="btn btn-primary" :disabled="prizeSaving" form="lottery-prize-form" type="submit">
          <Icon v-if="prizeSaving" name="refresh" size="sm" class="animate-spin" />
          <span>{{ prizeForm.id ? tr('actions.savePrize') : tr('actions.addPrize') }}</span>
        </button>
      </template>
    </BaseDialog>

    <BaseDialog
      :show="showGrantDialog"
      :title="activeCampaign ? `${tr('dialogs.grantChances')} · ${activeCampaign.name}` : tr('dialogs.grantChances')"
      width="wide"
      @close="closeGrantDialog"
    >
      <form id="lottery-grant-form" class="space-y-4" @submit.prevent="grantChances">
        <div>
          <label class="input-label">{{ tr('fields.selectUser') }}</label>
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
                @click="selectGrantUser(user)"
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
          <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
            {{ tr('hints.userSearch') }}
          </p>
        </div>

        <div v-if="selectedGrantUser" class="rounded-lg border border-primary-100 bg-primary-50 p-3 text-sm dark:border-primary-900/50 dark:bg-primary-900/20">
          <div class="font-medium text-primary-700 dark:text-primary-300">{{ selectedGrantUser.email }}</div>
          <div class="mt-1 text-primary-600 dark:text-primary-400">
            {{ selectedGrantUser.username || selectedGrantUser.notes || tr('userSelected') }} · #{{ selectedGrantUser.id }}
          </div>
        </div>

        <div class="grid gap-4 md:grid-cols-2">
          <div>
            <label class="input-label">{{ tr('fields.count') }}</label>
            <input v-model.number="grantForm.count" type="number" min="1" max="1000" class="input" />
          </div>
          <div>
            <label class="input-label">{{ tr('fields.expiresAt') }}</label>
            <input v-model="grantForm.expires_at" type="datetime-local" class="input" />
          </div>
        </div>
        <div class="rounded-lg bg-gray-50 p-3 text-sm text-gray-500 dark:bg-dark-800 dark:text-gray-400">
          {{ tr('hints.grantSource') }}
        </div>
      </form>
      <template #footer>
        <button class="btn btn-secondary" type="button" @click="closeGrantDialog">{{ t('common.cancel') }}</button>
        <button class="btn btn-primary" :disabled="!selectedGrantUser || granting" form="lottery-grant-form" type="submit">
          <Icon v-if="granting" name="refresh" size="sm" class="animate-spin" />
          <span>{{ tr('actions.grantChances') }}</span>
        </button>
      </template>
    </BaseDialog>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import DataTable from '@/components/common/DataTable.vue'
import Pagination from '@/components/common/Pagination.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Select, { type SelectOption } from '@/components/common/Select.vue'
import Icon from '@/components/icons/Icon.vue'
import { adminAPI } from '@/api/admin'
import lotteryAPI from '@/api/admin/lottery'
import { useAppStore } from '@/stores/app'
import { formatCurrency, formatDateTime } from '@/utils/format'
import { extractApiErrorMessage } from '@/utils/apiError'
import type { AdminGroup, AdminUser } from '@/types'
import type { Column } from '@/components/common/types'
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

const { t } = useI18n()
const appStore = useAppStore()

const loading = ref(false)
const prizesLoading = ref(false)
const campaignSaving = ref(false)
const prizeSaving = ref(false)
const granting = ref(false)
const userSearchLoading = ref(false)

const campaigns = ref<LotteryCampaign[]>([])
const prizes = ref<LotteryPrize[]>([])
const groups = ref<AdminGroup[]>([])
const activeCampaign = ref<LotteryCampaign | null>(null)
const statusFilter = ref<string | null>('')
const showCampaignDialog = ref(false)
const showPrizeManagerDialog = ref(false)
const showPrizeDialog = ref(false)
const showGrantDialog = ref(false)
const userSearchQuery = ref('')
const userSearchResults = ref<AdminUser[]>([])
const showUserResults = ref(false)
const selectedGrantUser = ref<AdminUser | null>(null)
let userSearchTimeout: ReturnType<typeof setTimeout> | null = null

const pagination = reactive({ page: 1, page_size: 20, total: 0, pages: 1 })

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
  redeem_group_id: null as number | null,
  redeem_validity_days: 30,
  min_value: 1,
  max_value: 10,
  sort_order: 0,
})

const grantForm = reactive({
  count: 1,
  expires_at: '',
})

const campaignColumns = computed<Column[]>(() => [
  { key: 'name', label: tr('fields.campaign'), sortable: true },
  { key: 'status', label: tr('fields.status'), sortable: true },
  { key: 'window', label: tr('columns.window') },
  { key: 'chance_expires_in_days', label: tr('fields.chanceTtl'), sortable: true },
  { key: 'actions', label: t('common.actions'), class: 'text-right' },
])

const prizeColumns = computed<Column[]>(() => [
  { key: 'name', label: tr('columns.prize'), sortable: true },
  { key: 'status', label: tr('fields.status'), sortable: true },
  { key: 'weight', label: tr('fields.weight'), sortable: true },
  { key: 'stock', label: tr('columns.stock') },
  { key: 'reward', label: tr('columns.reward') },
  { key: 'actions', label: t('common.actions'), class: 'text-right' },
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

const prizeStatusOptions = computed<SelectOption[]>(() => [
  { value: 'active', label: prizeStatusLabel('active') },
  { value: 'disabled', label: prizeStatusLabel('disabled') },
])

const rewardTypeOptions = computed<SelectOption[]>(() => [
  { value: 'balance', label: tr('rewardTypes.balance') },
  { value: 'concurrency', label: tr('rewardTypes.concurrency') },
  { value: 'subscription', label: tr('rewardTypes.subscription') },
  { value: 'invitation', label: tr('rewardTypes.invitation') },
  { value: 'timed_quota', label: tr('rewardTypes.timedQuota') },
  { value: 'random_timed_quota', label: tr('rewardTypes.randomTimedQuota') },
])

const subscriptionGroupOptions = computed<SelectOption[]>(() =>
  groups.value
    .filter((group) => group.subscription_type === 'subscription' || group.id === prizeForm.redeem_group_id)
    .map((group) => ({
      value: group.id,
      label: `${group.name} (#${group.id})`,
      description: [group.platform, group.status].filter(Boolean).join(' · '),
    }))
)

function tr(key: string, params?: Record<string, unknown>): string {
  return params ? t(`admin.lottery.${key}`, params) : t(`admin.lottery.${key}`)
}

function statusLabel(status: LotteryCampaignStatus | string): string {
  const labels: Record<string, string> = {
    draft: tr('status.draft'),
    active: tr('status.active'),
    disabled: tr('status.disabled'),
    ended: tr('status.ended'),
  }
  return labels[status] || status
}

function prizeStatusLabel(status: LotteryPrizeStatus | string): string {
  return status === 'active' ? tr('prizeStatus.active') : tr('prizeStatus.disabled')
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

function groupLabel(groupID: number | null | undefined): string {
  if (!groupID) return tr('noGroup')
  const group = groups.value.find((item) => item.id === groupID)
  return group ? `${group.name} (#${group.id})` : `#${groupID}`
}

function rewardLabel(prize: LotteryPrize): string {
  if (prize.redeem_type === 'balance' || prize.redeem_type === 'timed_quota') {
    const suffix = prize.redeem_type === 'timed_quota' ? ` · ${prize.redeem_validity_days}${tr('units.daysShort')}` : ''
    return `${formatCurrency(prize.redeem_value)}${suffix}`
  }
  if (prize.redeem_type === 'random_timed_quota') {
    const min = metadataNumber(prize.redeem_metadata, 'min_value', 0)
    const max = metadataNumber(prize.redeem_metadata, 'max_value', 0)
    return `${formatCurrency(min)} - ${formatCurrency(max)} · ${prize.redeem_validity_days}${tr('units.daysShort')}`
  }
  if (prize.redeem_type === 'subscription') return `${groupLabel(prize.redeem_group_id)} · ${prize.redeem_validity_days}${tr('units.daysShort')}`
  if (prize.redeem_type === 'concurrency') return `${prize.redeem_value} ${tr('rewardUnits.concurrency')}`
  return tr('rewardTypes.invitation')
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

function resetPrizeForm(): void {
  prizeForm.id = null
  prizeForm.name = ''
  prizeForm.description = ''
  prizeForm.status = 'active'
  prizeForm.weight = 1
  prizeForm.stock_total = 1
  prizeForm.redeem_type = 'balance'
  prizeForm.redeem_value = 0
  prizeForm.redeem_group_id = null
  prizeForm.redeem_validity_days = 30
  prizeForm.min_value = 1
  prizeForm.max_value = 10
  prizeForm.sort_order = 0
}

function resetGrantForm(): void {
  grantForm.count = 1
  grantForm.expires_at = ''
  userSearchQuery.value = ''
  userSearchResults.value = []
  showUserResults.value = false
  selectedGrantUser.value = null
}

async function loadCampaigns(page = pagination.page): Promise<void> {
  loading.value = true
  try {
    const result = await lotteryAPI.listCampaigns({ page, page_size: pagination.page_size, status: statusFilter.value || undefined })
    campaigns.value = result.items || []
    pagination.page = result.page
    pagination.page_size = result.page_size
    pagination.total = result.total
    pagination.pages = result.pages
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tr('errors.loadCampaigns')))
  } finally {
    loading.value = false
  }
}

async function loadGroups(): Promise<void> {
  try {
    groups.value = await adminAPI.groups.getAll()
  } catch (error) {
    console.error('Failed to load groups:', error)
  }
}

async function loadPrizes(campaignId: number): Promise<void> {
  prizesLoading.value = true
  try {
    prizes.value = await lotteryAPI.listPrizes(campaignId)
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tr('errors.loadPrizes')))
  } finally {
    prizesLoading.value = false
  }
}

function handleStatusFilterChange(): void {
  pagination.page = 1
  loadCampaigns(1)
}

function handlePageChange(page: number): void {
  loadCampaigns(page)
}

function handlePageSizeChange(pageSize: number): void {
  pagination.page_size = pageSize
  pagination.page = 1
  loadCampaigns(1)
}

function openCreateCampaignDialog(): void {
  resetCampaignForm()
  showCampaignDialog.value = true
}

function openEditCampaignDialog(campaign: LotteryCampaign): void {
  campaignForm.id = campaign.id
  campaignForm.name = campaign.name
  campaignForm.description = campaign.description || ''
  campaignForm.status = campaign.status
  campaignForm.starts_at = toLocalInput(campaign.starts_at)
  campaignForm.ends_at = toLocalInput(campaign.ends_at)
  campaignForm.chance_expires_in_days = campaign.chance_expires_in_days || 30
  showCampaignDialog.value = true
}

function closeCampaignDialog(): void {
  showCampaignDialog.value = false
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
      const updated = await lotteryAPI.updateCampaign(campaignForm.id, payload)
      if (activeCampaign.value?.id === updated.id) activeCampaign.value = updated
      appStore.showSuccess(tr('messages.campaignSaved'))
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
      activeCampaign.value = created
      appStore.showSuccess(tr('messages.campaignCreated'))
    }
    showCampaignDialog.value = false
    resetCampaignForm()
    await loadCampaigns()
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tr('errors.saveCampaign')))
  } finally {
    campaignSaving.value = false
  }
}

function openPrizeManager(campaign: LotteryCampaign): void {
  activeCampaign.value = campaign
  prizes.value = []
  resetPrizeForm()
  showPrizeManagerDialog.value = true
  loadPrizes(campaign.id)
}

function closePrizeManager(): void {
  showPrizeManagerDialog.value = false
  showPrizeDialog.value = false
}

function openCreatePrizeDialog(): void {
  resetPrizeForm()
  showPrizeDialog.value = true
}

function openEditPrizeDialog(prize: LotteryPrize): void {
  prizeForm.id = prize.id
  prizeForm.name = prize.name
  prizeForm.description = prize.description || ''
  prizeForm.status = prize.status
  prizeForm.weight = prize.weight
  prizeForm.stock_total = prize.stock_total
  prizeForm.redeem_type = prize.redeem_type
  prizeForm.redeem_value = prize.redeem_value
  prizeForm.redeem_group_id = prize.redeem_group_id ?? null
  prizeForm.redeem_validity_days = prize.redeem_validity_days || 30
  prizeForm.min_value = metadataNumber(prize.redeem_metadata, 'min_value', 1)
  prizeForm.max_value = metadataNumber(prize.redeem_metadata, 'max_value', 10)
  prizeForm.sort_order = prize.sort_order
  showPrizeDialog.value = true
}

function closePrizeDialog(): void {
  showPrizeDialog.value = false
}

function validatePrizeForm(): boolean {
  const weight = Number(prizeForm.weight)
  const stockTotal = Number(prizeForm.stock_total)
  const validityDays = Number(prizeForm.redeem_validity_days)
  if (!Number.isFinite(weight) || weight < 0 || !Number.isFinite(stockTotal) || stockTotal < 0) {
    appStore.showError(tr('errors.weightStockNonNegative'))
    return false
  }
  if (['subscription', 'timed_quota', 'random_timed_quota'].includes(prizeForm.redeem_type) && (!Number.isFinite(validityDays) || validityDays < 1)) {
    appStore.showError(tr('errors.validityDaysPositive'))
    return false
  }
  if (prizeForm.redeem_type === 'subscription' && !prizeForm.redeem_group_id) {
    appStore.showError(tr('errors.selectSubscriptionGroup'))
    return false
  }
  if (prizeForm.redeem_type === 'balance' || prizeForm.redeem_type === 'timed_quota') {
    const value = Number(prizeForm.redeem_value)
    if (!Number.isFinite(value) || value <= 0) {
      appStore.showError(tr('errors.rewardValuePositive'))
      return false
    }
  }
  if (prizeForm.redeem_type === 'random_timed_quota') {
    const min = Number(prizeForm.min_value)
    const max = Number(prizeForm.max_value)
    if (!Number.isFinite(min) || !Number.isFinite(max) || min <= 0 || max < min) {
      appStore.showError(tr('errors.randomTimedQuotaRange'))
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
  if (!activeCampaign.value || !prizeForm.name.trim()) return
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
      appStore.showSuccess(tr('messages.prizeSaved'))
    } else {
      await lotteryAPI.createPrize(activeCampaign.value.id, payload)
      appStore.showSuccess(tr('messages.prizeAdded'))
    }
    showPrizeDialog.value = false
    resetPrizeForm()
    await loadPrizes(activeCampaign.value.id)
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tr('errors.savePrize')))
  } finally {
    prizeSaving.value = false
  }
}

function openGrantDialog(campaign: LotteryCampaign): void {
  activeCampaign.value = campaign
  resetGrantForm()
  showGrantDialog.value = true
}

function closeGrantDialog(): void {
  showGrantDialog.value = false
  resetGrantForm()
}

function handleUserSearch(): void {
  if (userSearchTimeout) clearTimeout(userSearchTimeout)
  selectedGrantUser.value = null
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

function selectGrantUser(user: AdminUser): void {
  selectedGrantUser.value = user
  userSearchQuery.value = user.email
  userSearchResults.value = []
  showUserResults.value = false
}

function createGrantSourceID(): string {
  return `manual:${Date.now().toString(36)}:${Math.random().toString(36).slice(2, 8)}`
}

async function grantChances(): Promise<void> {
  if (!activeCampaign.value || !selectedGrantUser.value) return
  granting.value = true
  try {
    const expiresAt = localInputToISO(grantForm.expires_at)
    const granted = await lotteryAPI.grantChances(activeCampaign.value.id, {
      user_id: selectedGrantUser.value.id,
      count: Number(grantForm.count) || 1,
      source: 'admin',
      source_id: createGrantSourceID(),
      expires_at: expiresAt || undefined,
      metadata: { granted_from: 'admin_lottery_page' },
    })
    appStore.showSuccess(tr('messages.grantSuccess', { count: granted.length }))
    showGrantDialog.value = false
    resetGrantForm()
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, tr('errors.grantChances')))
  } finally {
    granting.value = false
  }
}

onMounted(() => {
  loadCampaigns()
  loadGroups()
})

onUnmounted(() => {
  if (userSearchTimeout) clearTimeout(userSearchTimeout)
})
</script>
