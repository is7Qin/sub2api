<template>
  <AppLayout>
    <TablePageLayout>
      <template #filters>
        <div class="flex flex-col justify-between gap-4 lg:flex-row lg:items-start">
          <div>
            <h1 class="text-2xl font-semibold text-gray-900 dark:text-white">
              {{ t('rankingReward.title') }}
            </h1>
            <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
              {{ t('rankingReward.description') }}
            </p>
          </div>
          <button
            @click="loadLeaderboards"
            :disabled="loading"
            class="btn btn-secondary"
            :title="t('common.refresh', 'Refresh')"
          >
            <Icon name="refresh" size="md" :class="loading ? 'animate-spin' : ''" />
          </button>
        </div>
      </template>

      <template #table>
        <div v-if="loading" class="space-y-4">
          <div v-for="i in 3" :key="i" class="card p-6">
            <div class="h-5 w-48 animate-pulse rounded bg-gray-200 dark:bg-dark-700" />
            <div class="mt-4 space-y-3">
              <div v-for="j in 4" :key="j" class="h-10 animate-pulse rounded bg-gray-100 dark:bg-dark-800" />
            </div>
          </div>
        </div>

        <div v-else-if="leaderboards.length === 0" class="card p-10 text-center">
          <Icon name="chart" size="xl" class="mx-auto text-gray-400 dark:text-gray-500" />
          <h2 class="mt-4 text-lg font-medium text-gray-900 dark:text-white">
            {{ t('rankingReward.emptyTitle') }}
          </h2>
          <p class="mt-2 text-sm text-gray-500 dark:text-gray-400">
            {{ t('rankingReward.emptyDescription') }}
          </p>
        </div>

        <div v-else class="space-y-6">
          <section v-for="board in leaderboards" :key="board.board_key" class="card overflow-hidden">
            <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
              <div class="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
                <div>
                  <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
                    {{ board.campaign_name }}
                  </h2>
                  <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
                    {{ t('rankingReward.rewardDate') }}: {{ formatDate(board.reward_date) }} · {{ t('rankingReward.displayLimit', { count: board.public_display_limit }) }}
                  </p>
                </div>
                <span class="inline-flex rounded-full bg-primary-50 px-3 py-1 text-xs font-medium text-primary-700 dark:bg-primary-900/30 dark:text-primary-300">
                  {{ t('rankingReward.awardedCount', { count: board.awarded_count }) }}
                </span>
              </div>
            </div>

            <div class="overflow-x-auto">
              <table class="min-w-full divide-y divide-gray-200 dark:divide-dark-700">
                <thead class="bg-gray-50 dark:bg-dark-800">
                  <tr>
                    <th class="px-6 py-3 text-left text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
                      {{ t('rankingReward.columns.rank') }}
                    </th>
                    <th class="px-6 py-3 text-left text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
                      {{ t('rankingReward.columns.user') }}
                    </th>
                    <th class="px-6 py-3 text-left text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
                      {{ t('rankingReward.columns.reward') }}
                    </th>
                    <th class="px-6 py-3 text-left text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">
                      {{ t('rankingReward.columns.status') }}
                    </th>
                  </tr>
                </thead>
                <tbody class="divide-y divide-gray-200 bg-white dark:divide-dark-700 dark:bg-dark-900">
                  <tr
                    v-for="entry in board.entries"
                    :key="`${board.board_key}-${entry.rank}`"
                    :class="entry.is_current_user ? 'bg-primary-50/60 dark:bg-primary-900/20' : ''"
                  >
                    <td class="whitespace-nowrap px-6 py-4 text-sm font-semibold text-gray-900 dark:text-white">
                      #{{ entry.rank }}
                    </td>
                    <td class="whitespace-nowrap px-6 py-4 text-sm text-gray-700 dark:text-gray-300">
                      <span>{{ t('rankingReward.anonymousUser', { code: entry.display_name }) }}</span>
                      <span v-if="entry.is_current_user" class="ml-2 rounded-full bg-primary-100 px-2 py-0.5 text-xs font-medium text-primary-700 dark:bg-primary-900/40 dark:text-primary-300">
                        {{ t('rankingReward.currentUser') }}
                      </span>
                    </td>
                    <td class="whitespace-nowrap px-6 py-4 text-sm text-gray-700 dark:text-gray-300">
                      {{ t('rankingReward.chanceCount', { count: entry.chance_count }) }}
                    </td>
                    <td class="whitespace-nowrap px-6 py-4 text-sm text-gray-700 dark:text-gray-300">
                      <span class="rounded-full bg-green-100 px-2 py-0.5 text-xs font-medium text-green-700 dark:bg-green-900/30 dark:text-green-300">
                        {{ t('rankingReward.awarded') }}
                      </span>
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
          </section>
        </div>
      </template>
    </TablePageLayout>
  </AppLayout>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import Icon from '@/components/icons/Icon.vue'
import rankingRewardAPI, { type PublicRankingRewardLeaderboard } from '@/api/rankingReward'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'

const { t, locale } = useI18n()
const appStore = useAppStore()

const leaderboards = ref<PublicRankingRewardLeaderboard[]>([])
const loading = ref(false)

function formatDate(value: string): string {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return new Intl.DateTimeFormat(String(locale.value)).format(date)
}

async function loadLeaderboards() {
  loading.value = true
  try {
    leaderboards.value = await rankingRewardAPI.getLeaderboards()
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('rankingReward.loadFailed')))
  } finally {
    loading.value = false
  }
}

onMounted(loadLeaderboards)
</script>
