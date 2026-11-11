<template>
  <div class="space-y-2">
    <label class="input-label">{{ t('admin.groups.webSearchPricing.account') }}</label>
    <Select :model-value="modelValue" :options="options" searchable remote :loading="loading"
      :aria-label="t('admin.groups.webSearchPricing.account')" @search="load" @update:model-value="emit('update:modelValue', Number($event) || 0)" />
    <p class="input-hint">{{ t('admin.groups.webSearchPricing.accountHint') }}</p>
    <p v-if="error" role="alert" class="text-sm text-red-600">{{ t('admin.groups.webSearchPricing.loadError') }}</p>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Select from './Select.vue'
import { adminAPI } from '@/api/admin'
const props = defineProps<{ modelValue: number; groupId: number }>()
const emit = defineEmits<{ 'update:modelValue': [value: number] }>()
const { t } = useI18n()
const accounts = ref<{ value: number; label: string; disabled?: boolean }[]>([])
const selectedName = ref('')
const loading = ref(false)
const error = ref(false)
let controller: AbortController | undefined
let generation = 0
const options = computed(() => [
  { value: 0, label: t('admin.groups.webSearchPricing.accountDefault') },
  ...(props.modelValue && !accounts.value.some(a => a.value === props.modelValue)
    ? [{ value: props.modelValue, label: selectedName.value || `#${props.modelValue}` }] : []),
  ...accounts.value
])
async function load(search = '') {
  controller?.abort()
  const current = ++generation
  controller = new AbortController()
  loading.value = true
  error.value = false
  try {
    const result = await adminAPI.accounts.list(1, 100, { platform: 'openai', group: String(props.groupId), search }, { signal: controller.signal })
    if (current !== generation) return
    accounts.value = result.items.map(a => ({ value: a.id, label: `${a.name} (#${a.id})`, disabled: a.status !== 'active' || a.extra?.openai_web_search_mode === 'disabled' }))
  } catch (err) {
    if (current === generation && !(err instanceof Error && err.name === 'AbortError')) error.value = true
  } finally {
    if (current === generation) loading.value = false
  }
}
watch(() => props.groupId, () => { accounts.value = []; void load() }, { immediate: true })
watch(() => props.modelValue, async id => {
  selectedName.value = ''
  if (!id) return
  const account = await adminAPI.accounts.getById(id).catch(() => null)
  if (props.modelValue === id && account) selectedName.value = `${account.name} (#${account.id})`
}, { immediate: true })
onBeforeUnmount(() => { ++generation; controller?.abort() })
</script>
