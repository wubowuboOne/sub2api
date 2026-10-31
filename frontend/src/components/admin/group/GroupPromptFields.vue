<template>
  <section class="border-t border-gray-200 pt-4 dark:border-dark-400" data-testid="group-prompt-fields">
    <div class="flex items-start justify-between gap-3">
      <div>
        <label class="input-label">{{ promptText('title') }}</label>
        <p class="input-hint">{{ promptText('hint') }}</p>
      </div>
      <Toggle v-model="enabled" :disabled="loading || !!loadError" :aria-label="promptText('title')" />
    </div>
    <p v-if="loading" class="input-hint">{{ promptText('loading') }}</p>
    <div v-else-if="loadError" class="mt-2 text-sm text-red-600" role="alert">
      {{ promptText('loadError') }}
      <button type="button" class="ml-2 underline" @click="load(groupId)">{{ promptText('retry') }}</button>
    </div>
    <div v-else-if="enabled" class="mt-3 space-y-2">
      <textarea
        v-model="content"
        class="input min-h-48 font-mono text-sm"
        rows="10"
        :aria-label="promptText('content')"
        data-testid="group-prompt-content"
      />
      <div class="flex items-center justify-between gap-2">
        <p class="input-hint">{{ promptText('defaultHint') }}</p>
        <button type="button" class="btn btn-secondary btn-sm shrink-0" @click="content = defaultContent">
          {{ promptText('reset') }}
        </button>
      </div>
      <div class="border-t border-gray-200 pt-3 dark:border-dark-400">
        <div class="flex items-center justify-between gap-2">
          <label class="input-label">{{ promptText('modelsTitle') }}</label>
          <button type="button" class="btn btn-secondary btn-sm" data-testid="add-model-prompt" @click="modelPrompts.push({ model: '', content: '' })">
            {{ promptText('addModel') }}
          </button>
        </div>
        <p class="input-hint">{{ promptText('modelsHint') }}</p>
        <div v-for="(rule, index) in modelPrompts" :key="index" class="mt-3 space-y-2 rounded-lg border border-gray-200 p-3 dark:border-dark-400" data-testid="model-prompt-row">
          <div class="flex items-center gap-2">
            <input v-model="rule.model" class="input flex-1" :aria-label="promptText('modelName')" :placeholder="promptText('modelName')" data-testid="model-prompt-name" />
            <button type="button" class="btn btn-secondary btn-sm shrink-0" data-testid="remove-model-prompt" @click="modelPrompts.splice(index, 1)">
              {{ promptText('removeModel') }}
            </button>
          </div>
          <textarea v-model="rule.content" class="input font-mono text-sm" rows="4" :aria-label="promptText('modelContent')" :placeholder="promptText('modelDefaultHint')" data-testid="model-prompt-content" />
        </div>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Toggle from '@/components/common/Toggle.vue'
import { getGroupPrompt, saveGroupPrompt } from '@/api/admin/groupPrompt'

const props = defineProps<{ groupId: number }>()
const { t: promptText } = useI18n({
  useScope: 'local',
  messages: {
    zh: {
      title: () => '前置提示词',
      hint: () => '开启后在模型请求中加入分组提示词，保留客户端原有内容。随分组保存生效。',
      content: () => '自定义前置提示词',
      defaultHint: () => '默认使用随程序打包的 system-prompt.md，支持自定义，最多 1 MiB。',
      modelsTitle: () => '按模型单独设置',
      modelsHint: () => '按客户端请求中的完整模型名精确匹配；未单独配置的模型使用上方分组提示词。全部配置合计最多 1 MiB。',
      addModel: () => '添加模型',
      removeModel: () => '删除',
      modelName: () => '完整模型名，例如 gpt-5.5',
      modelContent: () => '该模型的前置提示词',
      modelDefaultHint: () => '留空使用 system-prompt.md 默认内容',
      invalidModel: () => '请填写每条模型规则的完整模型名。',
      duplicateModel: () => '模型名不能重复，请合并同一模型的配置。',
      loading: () => '正在加载前置提示词…',
      loadError: () => '前置提示词加载失败，可重试；其他分组设置仍可保存。',
      retry: () => '重试',
      reset: () => '恢复默认内容',
    },
    en: {
      title: () => 'Prepend prompt',
      hint: () => 'Add a group prompt to model requests while preserving client content. Applied when the group is saved.',
      content: () => 'Custom prepend prompt',
      defaultHint: () => 'Defaults to the bundled system-prompt.md. Custom text supports up to 1 MiB.',
      modelsTitle: () => 'Per-model prompts',
      modelsHint: () => 'Exact match against the client-requested model name. Other models use the group prompt above. All settings combined support up to 1 MiB.',
      addModel: () => 'Add model',
      removeModel: () => 'Remove',
      modelName: () => 'Full model name, e.g. gpt-5.5',
      modelContent: () => 'Prompt for this model',
      modelDefaultHint: () => 'Leave empty to use the bundled system-prompt.md',
      invalidModel: () => 'Enter the full model name for every rule.',
      duplicateModel: () => 'Model names must be unique. Merge rules for the same model.',
      loading: () => 'Loading prompt…',
      loadError: () => 'Could not load the group prompt. Retry to edit it; other group settings can still be saved.',
      retry: () => 'Retry',
      reset: () => 'Restore default content',
    },
  },
})
const enabled = ref(false)
const content = ref('')
const defaultContent = ref('')
const modelPrompts = ref<Array<{ model: string; content: string }>>([])
const loading = ref(true)
const loadError = ref(false)
let generation = 0
let pending: Promise<void> = Promise.resolve()
let original = ''
const snapshot = () => JSON.stringify({ enabled: enabled.value, content: content.value, model_prompts: modelPrompts.value })

function load(id: number) {
  const current = ++generation
  loading.value = true
  loadError.value = false
  enabled.value = false
  content.value = ''
  modelPrompts.value = []
  pending = (async () => {
    try {
      const config = await getGroupPrompt(id)
      if (current !== generation) return
      enabled.value = config.enabled
      defaultContent.value = config.default_content
      content.value = config.content || config.default_content
      modelPrompts.value = Object.entries(config.model_prompts ?? {}).map(([model, content]) => ({ model, content }))
      original = snapshot()
    } catch {
      if (current === generation) loadError.value = true
    } finally {
      if (current === generation) loading.value = false
    }
  })()
  return pending
}

async function save() {
  // Fields cannot be edited while loading or after a failed load. Leave their
  // stored values alone and let the existing group form save without waiting.
  if (loading.value || loadError.value) return
  const entries = modelPrompts.value.map(rule => [rule.model.trim(), rule.content] as const)
  if (entries.some(([model]) => !model)) throw new Error(promptText('invalidModel'))
  if (new Set(entries.map(([model]) => model)).size !== entries.length) throw new Error(promptText('duplicateModel'))
  const current = snapshot()
  if (current === original) return
  await saveGroupPrompt(props.groupId, {
    enabled: enabled.value,
    content: content.value === defaultContent.value ? '' : content.value,
    ...(entries.length ? { model_prompts: Object.fromEntries(entries) } : {}),
  })
  original = current
}

watch(() => props.groupId, load, { immediate: true })
defineExpose({ save })
</script>
