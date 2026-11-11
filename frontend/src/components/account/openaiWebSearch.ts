export type OpenAIWebSearchMode = 'default' | 'only' | 'disabled'

export function readOpenAIWebSearchMode(extra?: Record<string, unknown>): OpenAIWebSearchMode {
  const value = extra?.openai_web_search_mode
  return value === 'only' || value === 'disabled' ? value : 'default'
}

export function applyOpenAIWebSearchMode(extra: Record<string, unknown>, mode: OpenAIWebSearchMode) {
  if (mode === 'default') delete extra.openai_web_search_mode
  else extra.openai_web_search_mode = mode
}
