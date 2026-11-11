import { describe, expect, it } from 'vitest'
import { applyOpenAIWebSearchMode, readOpenAIWebSearchMode } from '../openaiWebSearch'

describe('OpenAI standalone search mode', () => {
  it('keeps old accounts unchanged and accepts the two exclusive modes', () => {
    expect(readOpenAIWebSearchMode()).toBe('default')
    expect(readOpenAIWebSearchMode({ openai_web_search_mode: true })).toBe('default')
    expect(readOpenAIWebSearchMode({ openai_web_search_mode: 'only' })).toBe('only')
    expect(readOpenAIWebSearchMode({ openai_web_search_mode: 'disabled' })).toBe('disabled')
  })
  it('preserves all other extra fields and clears the default override', () => {
    const extra: Record<string, unknown> = { unrelated: true, openai_web_search_probe: { supported: true } }
    applyOpenAIWebSearchMode(extra, 'only')
    expect(extra.openai_web_search_mode).toBe('only')
    applyOpenAIWebSearchMode(extra, 'disabled')
    expect(extra.openai_web_search_mode).toBe('disabled')
    applyOpenAIWebSearchMode(extra, 'default')
    expect(extra).toEqual({ unrelated: true, openai_web_search_probe: { supported: true } })
  })
})
