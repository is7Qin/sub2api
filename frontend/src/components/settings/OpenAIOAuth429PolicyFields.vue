<template>
  <div class="space-y-4">
    <div class="flex items-center justify-between">
      <div>
        <label class="font-medium text-gray-900 dark:text-white">{{ t("admin.settings.openaiOAuth429Dynamic.enabled") }}</label>
        <p class="text-sm text-gray-500 dark:text-gray-400">{{ t("admin.settings.openaiOAuth429Dynamic.enabledHint") }}</p>
      </div>
      <Toggle v-model="policy.enabled" :data-testid="`${testIdPrefix}-enabled`" />
    </div>

    <div v-if="policy.enabled" class="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
      <div>
        <label class="mb-2 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t("admin.settings.openaiOAuth429Dynamic.windowSeconds") }}</label>
        <input v-model.number="policy.window_seconds" :data-testid="`${testIdPrefix}-window-seconds`" type="number" min="60" max="3600" class="input w-32" />
        <p class="mt-1.5 text-xs text-gray-500 dark:text-gray-400">{{ t("admin.settings.openaiOAuth429Dynamic.windowSecondsHint") }}</p>
      </div>
      <div>
        <label class="mb-2 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t("admin.settings.openaiOAuth429Dynamic.minSamples") }}</label>
        <input v-model.number="policy.min_samples" :data-testid="`${testIdPrefix}-min-samples`" type="number" min="2" max="10000" class="input w-32" />
        <p class="mt-1.5 text-xs text-gray-500 dark:text-gray-400">{{ t("admin.settings.openaiOAuth429Dynamic.minSamplesHint") }}</p>
      </div>
      <div>
        <label class="mb-2 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t("admin.settings.openaiOAuth429Dynamic.min429") }}</label>
        <input v-model.number="policy.min_429" :data-testid="`${testIdPrefix}-min-429`" type="number" min="1" :max="policy.min_samples" class="input w-32" />
        <p class="mt-1.5 text-xs text-gray-500 dark:text-gray-400">{{ t("admin.settings.openaiOAuth429Dynamic.min429Hint") }}</p>
      </div>
      <div>
        <label class="mb-2 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t("admin.settings.openaiOAuth429Dynamic.ratioThreshold") }}</label>
        <input v-model.number="policy.ratio_threshold" :data-testid="`${testIdPrefix}-ratio-threshold`" type="number" min="0.01" max="1" step="0.01" class="input w-32" />
        <p class="mt-1.5 text-xs text-gray-500 dark:text-gray-400">{{ t("admin.settings.openaiOAuth429Dynamic.ratioThresholdHint") }}</p>
      </div>
      <div>
        <label class="mb-2 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t("admin.settings.openaiOAuth429Dynamic.blockSeconds") }}</label>
        <input v-model.number="policy.block_seconds" :data-testid="`${testIdPrefix}-block-seconds`" type="number" min="1" max="2592000" class="input w-40" />
        <p class="mt-1.5 text-xs text-gray-500 dark:text-gray-400">{{ t("admin.settings.openaiOAuth429Dynamic.blockSecondsHint") }}</p>
      </div>
      <div class="col-span-full border-t border-gray-200 pt-4 dark:border-dark-600">
        <div class="flex items-center justify-between">
          <div>
            <label class="font-medium text-gray-900 dark:text-white">{{ t("admin.settings.openaiOAuth429Dynamic.usageWindowCheckEnabled") }}</label>
            <p class="text-sm text-gray-500 dark:text-gray-400">{{ t("admin.settings.openaiOAuth429Dynamic.usageWindowCheckHint") }}</p>
          </div>
          <Toggle v-model="policy.usage_window_check_enabled" :data-testid="`${testIdPrefix}-usage-window-check-enabled`" />
        </div>
        <div v-if="policy.usage_window_check_enabled" class="mt-3 grid gap-4 md:grid-cols-2">
          <div>
            <label class="mb-2 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t("admin.settings.openaiOAuth429Dynamic.usageWindow5hThreshold") }}</label>
            <input v-model.number="policy.usage_window_5h_threshold_percent" :data-testid="`${testIdPrefix}-usage-window-5h-threshold`" type="number" min="0.01" max="100" step="0.01" class="input w-32" />
          </div>
          <div>
            <label class="mb-2 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t("admin.settings.openaiOAuth429Dynamic.usageWindow7dThreshold") }}</label>
            <input v-model.number="policy.usage_window_7d_threshold_percent" :data-testid="`${testIdPrefix}-usage-window-7d-threshold`" type="number" min="0.01" max="100" step="0.01" class="input w-32" />
          </div>
          <div class="md:col-span-2">
            <label class="mb-2 block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t("admin.settings.openaiOAuth429Dynamic.usageWindowMissingDataFallbackSeconds") }}</label>
            <input v-model.number="policy.usage_window_missing_data_fallback_seconds" :data-testid="`${testIdPrefix}-usage-window-missing-data-fallback-seconds`" type="number" min="0" max="31536000" class="input w-40" />
            <p class="mt-1.5 text-xs text-gray-500 dark:text-gray-400">{{ t("admin.settings.openaiOAuth429Dynamic.usageWindowMissingDataFallbackSecondsHint") }}</p>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from "vue-i18n";
import Toggle from "@/components/common/Toggle.vue";
import type { OpenAIOAuth429DynamicPolicy } from "@/api/admin/settings";

defineProps<{
  policy: OpenAIOAuth429DynamicPolicy;
  testIdPrefix: string;
}>();

const { t } = useI18n();
</script>
