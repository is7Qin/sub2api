<template>
  <div class="space-y-5">
    <!-- Loading -->
    <div v-if="loading" class="card py-16 text-center">
      <Icon name="refresh" size="lg" class="inline-block animate-spin text-gray-400" />
    </div>

    <!-- Empty -->
    <div v-else-if="rows.length === 0" class="card py-16 text-center">
      <Icon name="inbox" size="xl" class="mx-auto mb-3 h-12 w-12 text-gray-400" />
      <p class="text-sm text-gray-500 dark:text-gray-400">{{ emptyLabel }}</p>
    </div>

    <!-- One card per channel -->
    <article
      v-for="(channel, chIdx) in rows"
      :key="`${channel.name}-${chIdx}`"
      class="card overflow-hidden p-0"
    >
      <!-- Channel header: name + description + platforms/groups (shown once) -->
      <header class="border-b border-gray-100 p-5 dark:border-dark-700">
        <div class="flex flex-wrap items-start justify-between gap-3">
          <div class="min-w-0">
            <div class="flex items-center gap-2.5">
              <span class="flex h-9 w-9 flex-none items-center justify-center rounded-xl bg-primary-500/10 text-primary-600 dark:bg-primary-500/15 dark:text-primary-400">
                <Icon name="server" size="md" :stroke-width="2" />
              </span>
              <div class="min-w-0">
                <h3 class="truncate text-base font-semibold text-gray-900 dark:text-white">{{ channel.name }}</h3>
                <p v-if="channel.description" class="truncate text-xs text-gray-500 dark:text-gray-400">{{ channel.description }}</p>
              </div>
            </div>
          </div>
          <span class="flex-none rounded-full bg-gray-100 px-2.5 py-1 text-xs font-medium text-gray-600 dark:bg-dark-700 dark:text-gray-300">
            {{ t('availableChannels.modelCount', { count: channelModels(channel).length }) }}
          </span>
        </div>

        <!-- Platforms + groups, one row each (deduped, not per-model) -->
        <div class="mt-4 flex flex-col gap-2.5">
          <div
            v-for="section in channel.platforms"
            :key="`${channel.name}-${section.platform}`"
            class="flex flex-wrap items-center gap-2"
          >
            <span
              :class="[
                'inline-flex items-center gap-1 rounded-md border px-2 py-0.5 text-[11px] font-medium uppercase',
                platformBadgeClass(section.platform),
              ]"
            >
              <PlatformIcon :platform="section.platform as GroupPlatform" size="xs" />
              {{ section.platform }}
            </span>
            <span class="text-gray-300 dark:text-dark-600">·</span>
            <template v-if="exclusiveGroups(section).length > 0">
              <span class="inline-flex items-center gap-0.5 text-[10px] font-medium uppercase text-purple-600 dark:text-purple-400" :title="t('availableChannels.exclusiveTooltip')">
                <Icon name="shield" size="xs" class="h-3 w-3" />
                {{ t('availableChannels.exclusive') }}
              </span>
              <GroupBadge
                v-for="g in exclusiveGroups(section)"
                :key="`ex-${g.id}`"
                :name="g.name"
                :platform="g.platform as GroupPlatform"
                :subscription-type="(g.subscription_type || 'standard') as SubscriptionType"
                :rate-multiplier="g.rate_multiplier"
                :user-rate-multiplier="userGroupRates[g.id] ?? null"
                always-show-rate
              />
            </template>
            <template v-if="publicGroups(section).length > 0">
              <span class="inline-flex items-center gap-0.5 text-[10px] font-medium uppercase text-gray-500 dark:text-gray-400" :title="t('availableChannels.publicTooltip')">
                <Icon name="globe" size="xs" class="h-3 w-3" />
                {{ t('availableChannels.public') }}
              </span>
              <GroupBadge
                v-for="g in publicGroups(section)"
                :key="`pub-${g.id}`"
                :name="g.name"
                :platform="g.platform as GroupPlatform"
                :subscription-type="(g.subscription_type || 'standard') as SubscriptionType"
                :rate-multiplier="g.rate_multiplier"
                :user-rate-multiplier="userGroupRates[g.id] ?? null"
                always-show-rate
              />
            </template>
          </div>
        </div>

        <!-- Per-channel model search + price-unit hint -->
        <div class="mt-4 flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
          <div class="relative w-full sm:w-72">
            <Icon name="search" size="md" class="pointer-events-none absolute left-3 top-1/2 z-10 -translate-y-1/2 text-gray-600 dark:text-gray-200" :stroke-width="2.25" />
            <input
              v-model="modelQuery[chIdx]"
              type="text"
              :placeholder="t('availableChannels.modelSearchPlaceholder')"
              class="input py-1.5 pl-10 text-sm"
            />
          </div>
          <p class="text-xs text-gray-400 dark:text-dark-500">{{ t('availableChannels.perMillionHint') }}</p>
        </div>
      </header>

      <!-- Inline model pricing table (deduped models, visible prices) -->
      <div class="overflow-x-auto">
        <table class="w-full min-w-[720px] border-collapse text-sm">
          <thead>
            <tr class="border-b border-gray-100 bg-gray-50/60 text-xs font-medium uppercase tracking-wide text-gray-500 dark:border-dark-700 dark:bg-dark-800/50 dark:text-gray-400">
              <th class="px-5 py-2.5 text-left">{{ t('availableChannels.modelColumn') }}</th>
              <th class="px-4 py-2.5 text-left">{{ t('availableChannels.billingColumn') }}</th>
              <th class="px-4 py-2.5 text-right">{{ t('availableChannels.priceInput') }}</th>
              <th class="px-4 py-2.5 text-right">{{ t('availableChannels.priceOutput') }}</th>
              <th class="px-4 py-2.5 text-right">{{ t('availableChannels.priceCacheRead') }}</th>
              <th class="px-5 py-2.5 text-right">{{ t('availableChannels.priceCacheWrite') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="m in visibleModels(channel, chIdx)"
              :key="`${m.platform}-${m.name}`"
              class="border-b border-gray-50 transition-colors last:border-b-0 hover:bg-gray-50/50 dark:border-dark-800 dark:hover:bg-dark-800/40"
            >
              <td class="px-5 py-3">
                <div class="flex items-center gap-2">
                  <PlatformIcon :platform="m.platform as GroupPlatform" size="xs" />
                  <span class="font-medium text-gray-900 dark:text-white">{{ m.name }}</span>
                </div>
              </td>
              <td class="px-4 py-3">
                <span class="inline-flex rounded-md bg-gray-100 px-2 py-0.5 text-[11px] font-medium text-gray-600 dark:bg-dark-700 dark:text-gray-300">
                  {{ billingLabel(m) }}
                </span>
              </td>
              <template v-if="isTokenBilling(m)">
                <td class="px-4 py-3 text-right font-mono text-gray-900 dark:text-gray-100">{{ perM(m.pricing?.input_price) }}</td>
                <td class="px-4 py-3 text-right font-mono text-gray-900 dark:text-gray-100">{{ perM(m.pricing?.output_price) }}</td>
                <td class="px-4 py-3 text-right font-mono text-sky-600 dark:text-sky-400">{{ perM(m.pricing?.cache_read_price) }}</td>
                <td class="px-5 py-3 text-right font-mono text-amber-600 dark:text-amber-400">{{ perM(m.pricing?.cache_write_price) }}</td>
              </template>
              <template v-else>
                <td class="px-4 py-3 text-right font-mono text-gray-900 dark:text-gray-100" colspan="4">
                  {{ nonTokenPrice(m) }}
                </td>
              </template>
            </tr>
            <tr v-if="visibleModels(channel, chIdx).length === 0">
              <td colspan="6" class="px-5 py-8 text-center text-sm text-gray-500 dark:text-gray-400">
                {{ modelQuery[chIdx] ? emptyLabel : t('availableChannels.noModelsInChannel') }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </article>
  </div>
</template>

<script setup lang="ts">
import { reactive } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import PlatformIcon from '@/components/common/PlatformIcon.vue'
import GroupBadge from '@/components/common/GroupBadge.vue'
import type { UserAvailableChannel, UserAvailableGroup, UserChannelPlatformSection, UserSupportedModel } from '@/api/channels'
import type { GroupPlatform, SubscriptionType } from '@/types'
import { platformBadgeClass } from '@/utils/platformColors'
import { formatScaled } from '@/utils/pricing'
import { BILLING_MODE_TOKEN, BILLING_MODE_PER_REQUEST, BILLING_MODE_IMAGE } from '@/constants/channel'

const props = defineProps<{
  columns: {
    name: string
    description: string
    platform: string
    groups: string
    supportedModels: string
  }
  rows: UserAvailableChannel[]
  loading: boolean
  pricingKeyPrefix: string
  noPricingLabel: string
  noModelsLabel: string
  emptyLabel: string
  /** 用户专属倍率（group_id → multiplier）。 */
  userGroupRates: Record<number, number>
}>()

void props.columns
void props.userGroupRates

const { t } = useI18n()

// Per-channel model search query, keyed by channel index.
const modelQuery = reactive<Record<number, string>>({})

const PER_MILLION = 1_000_000

function exclusiveGroups(section: UserChannelPlatformSection): UserAvailableGroup[] {
  return section.groups.filter((g) => g.is_exclusive)
}

function publicGroups(section: UserChannelPlatformSection): UserAvailableGroup[] {
  return section.groups.filter((g) => !g.is_exclusive)
}

// Deduplicate models within a channel: the same model shown across multiple
// platform sections is collapsed to a single row (keyed by platform+name).
function channelModels(channel: UserAvailableChannel): UserSupportedModel[] {
  const seen = new Map<string, UserSupportedModel>()
  for (const section of channel.platforms) {
    for (const m of section.supported_models) {
      const key = `${m.platform || section.platform}::${m.name}`
      if (!seen.has(key)) {
        seen.set(key, { ...m, platform: m.platform || section.platform })
      }
    }
  }
  return Array.from(seen.values()).sort((a, b) => a.name.localeCompare(b.name))
}

function visibleModels(channel: UserAvailableChannel, chIdx: number): UserSupportedModel[] {
  const q = (modelQuery[chIdx] || '').trim().toLowerCase()
  const all = channelModels(channel)
  if (!q) return all
  return all.filter((m) => m.name.toLowerCase().includes(q))
}

function isTokenBilling(m: UserSupportedModel): boolean {
  return m.pricing?.billing_mode === BILLING_MODE_TOKEN
}

function perM(value: number | null | undefined): string {
  if (value == null) return '-'
  return formatScaled(value, PER_MILLION)
}

function billingLabel(m: UserSupportedModel): string {
  const mode = m.pricing?.billing_mode
  switch (mode) {
    case BILLING_MODE_TOKEN: return t(`${props.pricingKeyPrefix}.billingModeToken`)
    case BILLING_MODE_PER_REQUEST: return t(`${props.pricingKeyPrefix}.billingModePerRequest`)
    case BILLING_MODE_IMAGE: return t(`${props.pricingKeyPrefix}.billingModeImage`)
    default: return props.noPricingLabel || '-'
  }
}

// For non-token billing (per-request / image), show the single relevant price
// spanning the price columns.
function nonTokenPrice(m: UserSupportedModel): string {
  const p = m.pricing
  if (!p) return props.noPricingLabel || '-'
  if (p.billing_mode === BILLING_MODE_PER_REQUEST && p.per_request_price != null) {
    return `${formatScaled(p.per_request_price, 1)} / ${t('availableChannels.pricePerRequest')}`
  }
  if (p.billing_mode === BILLING_MODE_IMAGE && p.per_request_price != null) {
    return `${formatScaled(p.per_request_price, 1)} / ${t('availableChannels.pricePerRequest')}`
  }
  return props.noPricingLabel || '-'
}
</script>
