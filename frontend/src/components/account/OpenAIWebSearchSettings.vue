<template>
  <div class="border-t border-gray-200 pt-4 dark:border-dark-600 space-y-3">
    <div class="flex items-center justify-between gap-4">
      <label :for="`${uid}-only`" class="input-label mb-0">{{ t('admin.accounts.openai.webSearchOnly') }}</label>
      <Toggle :id="`${uid}-only`" :model-value="modelValue === 'only'" :aria-label="t('admin.accounts.openai.webSearchOnly')"
        @update:model-value="emit('update:modelValue', $event ? 'only' : 'default')" />
    </div>
    <div class="flex items-center justify-between gap-4">
      <label :for="`${uid}-disabled`" class="input-label mb-0">{{ t('admin.accounts.openai.webSearchDisabled') }}</label>
      <Toggle :id="`${uid}-disabled`" :model-value="modelValue === 'disabled'" :aria-label="t('admin.accounts.openai.webSearchDisabled')"
        @update:model-value="emit('update:modelValue', $event ? 'disabled' : 'default')" />
    </div>
    <p class="input-hint">{{ t('admin.accounts.openai.webSearchRoutingHint') }}</p>
    <template v-if="account">
      <button type="button" class="btn btn-secondary btn-sm" @click="showProbe = true">
        <Icon name="play" size="sm" />{{ t('admin.accounts.openai.webSearchProbe') }}
      </button>
      <p v-if="probe" class="text-xs text-gray-500 break-words">
        {{ t('admin.accounts.openai.webSearchLastProbe') }}:
        {{ t(probe.supported === true ? 'admin.accounts.openai.webSearchSupported' : probe.supported === false ? 'admin.accounts.openai.webSearchUnsupported' : 'admin.accounts.openai.webSearchUnknown') }}
        · {{ probe.model }} · {{ typeof probe.checked_at === 'string' ? formatDateTime(probe.checked_at) : '' }}
        <span v-if="probe.error"> · {{ probe.error }}</span>
      </p>
      <AccountTestModal :show="showProbe" :account="account" initial-mode="web_search" @close="showProbe = false" @tested="refreshProbe" />
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, getCurrentInstance, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Toggle from '@/components/common/Toggle.vue'
import { Icon } from '@/components/icons'
import AccountTestModal from './AccountTestModal.vue'
import { adminAPI } from '@/api/admin'
import type { Account } from '@/types'
import type { OpenAIWebSearchMode } from './openaiWebSearch'
import { formatDateTime } from '@/utils/format'

const props = defineProps<{ modelValue: OpenAIWebSearchMode; account?: Account | null }>()
const emit = defineEmits<{ 'update:modelValue': [value: OpenAIWebSearchMode] }>()
const { t } = useI18n()
const uid = `web-search-${getCurrentInstance()?.uid}`
const showProbe = ref(false)
const probeOverride = ref<Record<string, unknown> | null>(null)
const probe = computed(() => probeOverride.value ?? props.account?.extra?.openai_web_search_probe as Record<string, unknown> | undefined)
watch(() => props.account?.id, () => { probeOverride.value = null; showProbe.value = false })
async function refreshProbe() {
  const id = props.account?.id
  if (!id) return
  const account = await adminAPI.accounts.getById(id).catch(() => null)
  if (props.account?.id === id && account) probeOverride.value = account.extra?.openai_web_search_probe as Record<string, unknown> ?? null
}
</script>
