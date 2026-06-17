<template>
  <div class="min-h-screen bg-gray-50 dark:bg-dark-950">
    <div class="pointer-events-none fixed inset-0 bg-mesh-gradient opacity-80"></div>
    <main class="relative flex min-h-screen items-center justify-center px-4 py-10">
      <section class="w-full max-w-xl rounded-2xl border border-white/80 bg-white/95 p-8 shadow-[0_24px_80px_rgba(15,23,42,0.14)] backdrop-blur-xl dark:border-dark-700 dark:bg-dark-900/95">
        <div class="mx-auto flex w-fit items-center gap-3 rounded-2xl bg-gray-50 px-4 py-3 dark:bg-dark-800">
          <img
            v-if="siteLogo"
            :src="siteLogo"
            :alt="siteName"
            class="h-11 w-11 rounded-xl object-cover"
          >
          <div
            v-else
            class="flex h-11 w-11 items-center justify-center rounded-xl bg-primary-600 text-white"
          >
            <Icon name="globe" size="md" />
          </div>
          <div>
            <p class="text-base font-semibold text-gray-900 dark:text-white">{{ siteName }}</p>
            <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('regionGate.subtitle') }}</p>
          </div>
        </div>

        <div class="mt-8 flex flex-col items-center text-center">
          <div class="flex h-20 w-20 items-center justify-center rounded-full bg-red-50 text-red-600 ring-4 ring-red-100 dark:bg-red-950/30 dark:text-red-400 dark:ring-red-950/60">
            <Icon name="ban" size="xl" :stroke-width="2" />
          </div>
          <h1 class="mt-6 text-3xl font-semibold tracking-tight text-gray-900 dark:text-white">
            {{ t('regionGate.title') }}
          </h1>
          <p class="mt-4 max-w-lg text-sm leading-7 text-gray-600 dark:text-gray-300">
            {{ t('regionGate.notice') }}
          </p>

          <div class="mt-5 text-sm text-gray-500 dark:text-gray-400">
            <template v-if="checking">
              {{ t('regionGate.detecting') }}
            </template>
            <template v-else-if="countryLabel">
              {{ t('regionGate.detected', { country: countryLabel }) }}
            </template>
            <template v-else>
              {{ t('regionGate.fallback') }}
            </template>
          </div>

          <div class="mt-6 w-full rounded-2xl border border-gray-200 bg-gray-50/90 p-4 text-sm leading-7 text-gray-600 dark:border-dark-700 dark:bg-dark-800/80 dark:text-gray-300">
            {{ t('regionGate.supportHint') }}
          </div>

          <button
            type="button"
            class="btn btn-primary mt-6 w-full max-w-sm"
            :disabled="checking"
            @click="handleRecheck"
          >
            <LoadingSpinner v-if="checking" size="sm" color="white" class="mr-2" />
            <span>{{ checking ? t('regionGate.rechecking') : t('regionGate.retry') }}</span>
          </button>
        </div>
      </section>
    </main>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import LoadingSpinner from '@/components/common/LoadingSpinner.vue'
import Icon from '@/components/icons/Icon.vue'
import { useAppStore, useGeoGateStore } from '@/stores'

const { t } = useI18n()
const appStore = useAppStore()
const geoGateStore = useGeoGateStore()

const siteName = computed(() => appStore.siteName || 'Sub2API')
const siteLogo = computed(() => appStore.siteLogo || '')
const checking = computed(() => geoGateStore.checking)
const countryLabel = computed(() => {
  const countryName = geoGateStore.countryName.trim()
  const countryCode = geoGateStore.countryCode.trim()
  if (countryName) {
    return countryCode ? `${countryName} (${countryCode})` : countryName
  }
  return countryCode
})

async function handleRecheck() {
  await geoGateStore.recheck()
}
</script>
