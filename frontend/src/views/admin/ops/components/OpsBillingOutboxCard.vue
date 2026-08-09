<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import type { OpsBillingOutboxHealth } from '@/api/admin/ops'
import { formatNumber } from '@/utils/format'

interface Props {
  health: OpsBillingOutboxHealth | null
  loading: boolean
}

const props = withDefaults(defineProps<Props>(), {
  health: null,
  loading: false
})

const { t } = useI18n()

const showDetails = ref(false)

const backlog = computed(() => {
  const h = props.health
  return h ? (h.pending ?? 0) + (h.processing ?? 0) : 0
})

// oldest_lag 为纳秒整数；≥1h 显示 "Xh Ym"，否则 "Xm"
function formatLag(ns: number): string {
  const totalSec = Math.max(0, Math.floor(ns / 1e9))
  const hours = Math.floor(totalSec / 3600)
  const minutes = Math.floor((totalSec % 3600) / 60)
  if (hours > 0) return `${hours}h ${minutes}m`
  return `${minutes}m`
}

const lagClass = computed(() => {
  const ns = props.health?.oldest_lag ?? 0
  if (ns > 3600 * 1e9) return 'text-red-600'
  if (ns > 600 * 1e9) return 'text-yellow-600'
  return ''
})
</script>

<template>
  <!-- 独立瘦行形态（不与顶行图表挤列宽）：glass 底色 + 横排；明细折叠展开占整行 -->
  <div class="flex flex-col gap-3 rounded-2xl border border-gray-200/70 bg-[var(--glass-bg-content)] p-4 shadow-sm dark:border-white/10 lg:flex-row lg:items-center lg:justify-between lg:gap-6">
    <div class="flex shrink-0 items-center gap-2">
      <h3 class="flex items-center gap-2 text-sm font-bold text-gray-900 dark:text-white">{{ t('admin.ops.billingOutbox.title') }}</h3>
      <span v-if="loading" class="billing-card-loading h-4 w-16 animate-pulse rounded bg-gray-200 dark:bg-gray-700"></span>
    </div>

    <template v-if="!loading && health">
      <div class="flex flex-wrap items-center gap-2 text-xs">
        <span
          :class="health.running ? 'bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-300' : 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300'"
          class="rounded-md px-2 py-1 font-medium"
        >
          {{ health.running ? t('admin.ops.billingOutbox.running') : t('admin.ops.billingOutbox.stopped') }}
        </span>
        <span
          :class="health.circuit_open ? 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300' : 'bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-300'"
          class="rounded-md px-2 py-1 font-medium"
          :title="health.circuit_error"
        >
          {{ health.circuit_open ? t('admin.ops.billingOutbox.circuitOpen') : t('admin.ops.billingOutbox.circuitClosed') }}
          <!-- 熔断时显示截断的错误文本（可见），title 保留全量 -->
          <span v-if="health.circuit_open && health.circuit_error" class="ml-1 inline-block max-w-[200px] truncate">{{ health.circuit_error }}</span>
        </span>
      </div>

      <div class="flex flex-wrap items-center gap-4 text-center">
        <div class="metric-backlog">
          <div class="text-lg font-bold leading-tight text-gray-900 dark:text-white">{{ formatNumber(backlog) }}</div>
          <div class="text-[10px] text-gray-500 dark:text-gray-400">{{ t('admin.ops.billingOutbox.backlog') }}</div>
        </div>
        <div class="metric-lag">
          <div class="lag-value text-lg font-bold leading-tight" :class="lagClass">{{ formatLag(health.oldest_lag) }}</div>
          <div class="text-[10px] text-gray-500 dark:text-gray-400">{{ t('admin.ops.billingOutbox.lag') }}</div>
        </div>
        <div class="metric-terminal">
          <div class="text-lg font-bold leading-tight" :class="health.terminal_alert ? 'text-orange-600' : 'text-gray-900 dark:text-white'">{{ formatNumber(health.terminal) }}</div>
          <div class="text-[10px] text-gray-500 dark:text-gray-400">{{ t('admin.ops.billingOutbox.terminal') }}</div>
        </div>
      </div>

      <div v-if="health.terminal_alert" class="terminal-alert rounded-md bg-orange-50 px-2 py-1 text-[11px] text-orange-600 dark:bg-orange-900/20 dark:text-orange-300">
        {{ health.terminal_alert }}
      </div>

      <button class="billing-details-toggle shrink-0 text-[11px] font-medium text-blue-600 dark:text-blue-400" @click="showDetails = !showDetails">
        {{ showDetails ? t('admin.ops.billingOutbox.details.hide') : t('admin.ops.billingOutbox.details.show') }}
      </button>
      <div v-if="showDetails" class="w-full space-y-1 border-t border-gray-200/70 pt-2 text-[11px] text-gray-500 dark:border-white/10 dark:text-gray-400 lg:order-last">
        <div class="text-[10px] text-gray-400">{{ t('admin.ops.billingOutbox.instanceView') }}</div>
        <div>{{ t('admin.ops.billingOutbox.details.processed') }}: {{ formatNumber(health.processed) }}</div>
        <div>{{ t('admin.ops.billingOutbox.details.failures') }}: {{ formatNumber(health.failures) }}</div>
        <div>{{ t('admin.ops.billingOutbox.details.backloggedRounds') }}: {{ formatNumber(health.backlogged_rounds) }}</div>
        <div>{{ t('admin.ops.billingOutbox.details.roundTimeouts') }}: {{ formatNumber(health.round_timeouts) }}</div>
        <div>{{ t('admin.ops.billingOutbox.details.permanentFailures') }}: {{ formatNumber(health.permanent_failures) }}</div>
        <div>{{ t('admin.ops.billingOutbox.details.maxAttempts') }}: {{ health.max_attempts }}</div>
        <div v-if="health.circuit_opened_at">{{ t('admin.ops.billingOutbox.details.circuitOpenedAt') }}: {{ health.circuit_opened_at }}</div>
        <div v-if="health.last_error">{{ t('admin.ops.billingOutbox.details.lastError') }}: {{ health.last_error }}</div>
        <div v-if="health.stats_error">{{ t('admin.ops.billingOutbox.details.statsError') }}: {{ health.stats_error }}</div>
      </div>
    </template>

    <div v-else-if="!loading" class="text-xs text-gray-500 dark:text-gray-400">
      {{ t('admin.ops.billingOutbox.error') }}
    </div>
  </div>
</template>
