<template>
  <AppLayout>
    <div class="space-y-6">
      <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div>
          <h1 class="text-2xl font-bold text-gray-900 dark:text-white">{{ t('lottery.title') }}</h1>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('lottery.description') }}</p>
        </div>
        <button class="btn btn-secondary" :disabled="loading" @click="loadAll()">
          <Icon name="refresh" size="sm" :class="loading ? 'animate-spin' : ''" />
          <span>{{ t('common.refresh') }}</span>
        </button>
      </div>

      <div class="grid gap-4 sm:grid-cols-3">
        <div class="card p-5">
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('lottery.stats.activeCampaigns') }}</p>
          <p class="mt-2 text-2xl font-semibold text-gray-900 dark:text-white">{{ campaigns.length }}</p>
        </div>
        <div class="card p-5">
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('lottery.stats.availableChances') }}</p>
          <p class="mt-2 text-2xl font-semibold text-primary-600 dark:text-primary-400">{{ availableChanceCount }}</p>
        </div>
        <div class="card p-5">
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('lottery.stats.awardedDraws') }}</p>
          <p class="mt-2 text-2xl font-semibold text-emerald-600 dark:text-emerald-400">{{ awardedDrawCount }}</p>
        </div>
      </div>

      <div v-if="loading" class="flex justify-center py-12">
        <div class="h-8 w-8 animate-spin rounded-full border-2 border-primary-500 border-t-transparent"></div>
      </div>

      <template v-else>
        <div class="card overflow-hidden">
          <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
            <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('lottery.campaigns.title') }}</h2>
          </div>
          <div v-if="campaigns.length === 0" class="p-8 text-center text-sm text-gray-500 dark:text-gray-400">
            {{ t('lottery.campaigns.empty') }}
          </div>
          <div v-else class="grid gap-4 p-6 lg:grid-cols-2">
            <div
              v-for="campaign in campaigns"
              :key="campaign.id"
              class="rounded-2xl border border-gray-200 p-5 dark:border-dark-700"
            >
              <div class="flex items-start justify-between gap-3">
                <div class="min-w-0">
                  <h3 class="truncate text-base font-semibold text-gray-900 dark:text-white">{{ campaign.name }}</h3>
                  <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ campaign.description || t('lottery.campaigns.noDescription') }}</p>
                </div>
                <span class="rounded-full bg-emerald-100 px-2 py-0.5 text-xs font-medium text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300">
                  {{ t('lottery.status.active') }}
                </span>
              </div>
              <div class="mt-4 space-y-2 text-sm text-gray-600 dark:text-gray-300">
                <p>{{ t('lottery.campaigns.endsAt') }}: {{ formatDateTime(campaign.ends_at) || t('lottery.campaigns.noEnd') }}</p>
                <p>{{ t('lottery.campaigns.chanceExpires') }}: {{ campaign.chance_expires_in_days }} {{ t('lottery.days') }}</p>
                <p>{{ t('lottery.campaigns.availableChances') }}: {{ availableChancesFor(campaign.id).length }}</p>
              </div>
              <div v-if="campaign.prizes?.length" class="mt-4 rounded-xl bg-gray-50 p-3 dark:bg-dark-800">
                <p class="mb-2 text-xs font-medium uppercase tracking-wide text-gray-500 dark:text-gray-400">{{ t('lottery.campaigns.prizes') }}</p>
                <div class="space-y-1 text-sm text-gray-700 dark:text-gray-300">
                  <p v-for="prize in campaign.prizes" :key="prize.id" class="flex justify-between gap-3">
                    <span class="truncate">{{ prize.name }}</span>
                    <span class="shrink-0 text-gray-500 dark:text-gray-400">{{ rewardLabel(prize) }}</span>
                  </p>
                </div>
              </div>
              <button
                class="btn btn-primary mt-4 w-full"
                :disabled="drawingCampaignId === campaign.id || availableChancesFor(campaign.id).length === 0"
                @click="draw(campaign.id)"
              >
                <Icon v-if="drawingCampaignId === campaign.id" name="refresh" size="sm" class="animate-spin" />
                <Icon v-else name="sparkles" size="sm" />
                <span>{{ availableChancesFor(campaign.id).length > 0 ? t('lottery.draw.button') : t('lottery.draw.noChance') }}</span>
              </button>
            </div>
          </div>
        </div>

        <div v-if="lastResult" class="card border-primary-200 bg-primary-50 p-6 dark:border-primary-900/40 dark:bg-primary-900/20">
          <div class="flex items-start gap-3">
            <Icon name="sparkles" size="lg" class="text-primary-600 dark:text-primary-300" />
            <div>
              <h2 class="text-lg font-semibold text-primary-900 dark:text-primary-100">{{ t('lottery.draw.resultTitle') }}</h2>
              <p class="mt-1 text-primary-800 dark:text-primary-200">
                {{ t('lottery.draw.wonPrize', { prize: lastResult.prize?.name || '-' }) }}
              </p>
              <p v-if="lastResult.draw?.redeem_code" class="mt-3 rounded-lg bg-white px-3 py-2 font-mono text-sm text-primary-900 dark:bg-dark-900 dark:text-primary-100">
                {{ lastResult.draw.redeem_code }}
              </p>
            </div>
          </div>
        </div>

        <div class="grid gap-6 xl:grid-cols-2">
          <div class="card overflow-hidden">
            <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
              <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('lottery.chances.title') }}</h2>
            </div>
            <div v-if="chances.length === 0" class="p-8 text-center text-sm text-gray-500 dark:text-gray-400">
              {{ t('lottery.chances.empty') }}
            </div>
            <div v-else class="overflow-x-auto">
              <table class="w-full min-w-[520px] text-left text-sm">
                <thead>
                  <tr class="border-b border-gray-100 text-gray-500 dark:border-dark-700 dark:text-gray-400">
                    <th class="px-6 py-3 font-medium">{{ t('lottery.columns.campaign') }}</th>
                    <th class="px-4 py-3 font-medium">{{ t('lottery.columns.status') }}</th>
                    <th class="px-4 py-3 font-medium">{{ t('lottery.columns.source') }}</th>
                    <th class="px-6 py-3 font-medium">{{ t('lottery.columns.expiresAt') }}</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="chance in chances" :key="chance.id" class="border-b border-gray-100 last:border-b-0 dark:border-dark-800">
                    <td class="px-6 py-4 text-gray-900 dark:text-white">{{ campaignName(chance.campaign_id) }}</td>
                    <td class="px-4 py-4">
                      <span class="rounded-full px-2 py-0.5 text-xs font-medium" :class="chanceStatusClass(chance.status)">
                        {{ chanceStatusLabel(chance.status) }}
                      </span>
                    </td>
                    <td class="px-4 py-4 text-gray-700 dark:text-gray-300">{{ chance.source || '-' }}</td>
                    <td class="px-6 py-4 text-gray-700 dark:text-gray-300">{{ formatDateTime(chance.expires_at) }}</td>
                  </tr>
                </tbody>
              </table>
            </div>
          </div>

          <div class="card overflow-hidden">
            <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
              <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('lottery.draws.title') }}</h2>
            </div>
            <div v-if="draws.length === 0" class="p-8 text-center text-sm text-gray-500 dark:text-gray-400">
              {{ t('lottery.draws.empty') }}
            </div>
            <div v-else class="overflow-x-auto">
              <table class="w-full min-w-[620px] text-left text-sm">
                <thead>
                  <tr class="border-b border-gray-100 text-gray-500 dark:border-dark-700 dark:text-gray-400">
                    <th class="px-6 py-3 font-medium">{{ t('lottery.columns.prize') }}</th>
                    <th class="px-4 py-3 font-medium">{{ t('lottery.columns.status') }}</th>
                    <th class="px-4 py-3 font-medium">{{ t('lottery.columns.redeemCode') }}</th>
                    <th class="px-6 py-3 font-medium">{{ t('lottery.columns.drawnAt') }}</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="item in draws" :key="item.id" class="border-b border-gray-100 last:border-b-0 dark:border-dark-800">
                    <td class="px-6 py-4 text-gray-900 dark:text-white">{{ item.prize?.name || '-' }}</td>
                    <td class="px-4 py-4">
                      <span class="rounded-full px-2 py-0.5 text-xs font-medium" :class="drawStatusClass(item.status)">
                        {{ drawStatusLabel(item.status) }}
                      </span>
                    </td>
                    <td class="px-4 py-4 font-mono text-gray-700 dark:text-gray-300">{{ item.redeem_code || '-' }}</td>
                    <td class="px-6 py-4 text-gray-700 dark:text-gray-300">{{ formatDateTime(item.drawn_at) }}</td>
                  </tr>
                </tbody>
              </table>
            </div>
          </div>
        </div>
      </template>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import Icon from '@/components/icons/Icon.vue'
import lotteryAPI from '@/api/lottery'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatCurrency, formatDateTime } from '@/utils/format'
import type { LotteryCampaign, LotteryChance, LotteryDraw, LotteryDrawResult, LotteryPrize } from '@/api/admin/lottery'

const { t } = useI18n()
const appStore = useAppStore()

const loading = ref(false)
const drawingCampaignId = ref<number | null>(null)
const campaigns = ref<LotteryCampaign[]>([])
const chances = ref<LotteryChance[]>([])
const draws = ref<LotteryDraw[]>([])
const lastResult = ref<LotteryDrawResult | null>(null)

const availableChanceCount = computed(() => chances.value.filter((item) => item.status === 'available').length)
const awardedDrawCount = computed(() => draws.value.filter((item) => item.status === 'awarded').length)

function availableChancesFor(campaignId: number): LotteryChance[] {
  return chances.value.filter((item) => item.campaign_id === campaignId && item.status === 'available')
}

function campaignName(campaignId: number): string {
  return campaigns.value.find((item) => item.id === campaignId)?.name || `#${campaignId}`
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
  if (prize.redeem_type === 'balance' || prize.redeem_type === 'timed_quota') return formatCurrency(prize.redeem_value)
  if (prize.redeem_type === 'random_timed_quota') {
    const min = metadataNumber(prize.redeem_metadata, 'min_value', 0)
    const max = metadataNumber(prize.redeem_metadata, 'max_value', 0)
    return `${formatCurrency(min)} - ${formatCurrency(max)}`
  }
  if (prize.redeem_type === 'subscription') return `${prize.redeem_validity_days} ${t('lottery.days')}`
  if (prize.redeem_type === 'concurrency') return String(prize.redeem_value)
  return t('lottery.rewardTypes.invitation')
}

function chanceStatusLabel(status: string): string {
  return t(`lottery.chanceStatus.${status}`)
}

function drawStatusLabel(status: string): string {
  return t(`lottery.drawStatus.${status}`)
}

function chanceStatusClass(status: string): string {
  if (status === 'available') return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300'
  if (status === 'used') return 'bg-blue-100 text-blue-700 dark:bg-blue-900/40 dark:text-blue-300'
  return 'bg-gray-100 text-gray-700 dark:bg-dark-700 dark:text-gray-300'
}

function drawStatusClass(status: string): string {
  if (status === 'awarded') return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300'
  if (status === 'failed') return 'bg-red-100 text-red-700 dark:bg-red-900/40 dark:text-red-300'
  return 'bg-blue-100 text-blue-700 dark:bg-blue-900/40 dark:text-blue-300'
}

async function loadAll(silent = false): Promise<void> {
  if (!silent) loading.value = true
  try {
    const [campaignResult, chanceResult, drawResult] = await Promise.all([
      lotteryAPI.listCampaigns({ page: 1, page_size: 50 }),
      lotteryAPI.listChances({ page: 1, page_size: 100 }),
      lotteryAPI.listDraws({ page: 1, page_size: 100 }),
    ])
    campaigns.value = campaignResult.items || []
    chances.value = chanceResult.items || []
    draws.value = drawResult.items || []
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, t('lottery.loadFailed')))
  } finally {
    if (!silent) loading.value = false
  }
}

async function draw(campaignId: number): Promise<void> {
  if (drawingCampaignId.value) return
  drawingCampaignId.value = campaignId
  try {
    lastResult.value = await lotteryAPI.draw(campaignId)
    appStore.showSuccess(t('lottery.draw.success', { prize: lastResult.value.prize?.name || '-' }))
    await loadAll(true)
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, t('lottery.draw.failed')))
  } finally {
    drawingCampaignId.value = null
  }
}

onMounted(() => {
  loadAll()
})
</script>
