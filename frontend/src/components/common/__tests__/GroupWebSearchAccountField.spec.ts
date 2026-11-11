import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import GroupWebSearchAccountField from '../GroupWebSearchAccountField.vue'

const { list, getById } = vi.hoisted(() => ({ list: vi.fn(), getById: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { list, getById } } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

describe('GroupWebSearchAccountField', () => {
  beforeEach(() => { list.mockReset(); getById.mockReset(); getById.mockResolvedValue(null) })
  it('loads only OpenAI members and excludes accounts with disabled search', async () => {
    list.mockResolvedValue({ items: [
      { id: 7, name: 'search', status: 'active', extra: { openai_web_search_mode: 'only' } },
      { id: 8, name: 'off', status: 'active', extra: { openai_web_search_mode: 'disabled' } }
    ] })
    const wrapper = mount(GroupWebSearchAccountField, { props: { modelValue: 0, groupId: 42 } })
    await flushPromises()
    expect(list).toHaveBeenCalledWith(1, 100, { platform: 'openai', group: '42', search: '' }, { signal: expect.any(AbortSignal) })
    const select = wrapper.findComponent({ name: 'Select' })
    expect(select.props('options')).toEqual([
      { value: 0, label: 'admin.groups.webSearchPricing.accountDefault' },
      { value: 7, label: 'search (#7)', disabled: false },
      { value: 8, label: 'off (#8)', disabled: true }
    ])
    select.vm.$emit('update:modelValue', 7)
    expect(wrapper.emitted('update:modelValue')).toEqual([[7]])
    wrapper.unmount()
  })
  it('keeps the configured ID when loading fails', async () => {
    list.mockRejectedValue(new Error('network'))
    const wrapper = mount(GroupWebSearchAccountField, { props: { modelValue: 9, groupId: 42 } })
    await flushPromises()
    expect(wrapper.find('[role="alert"]').exists()).toBe(true)
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(wrapper.findComponent({ name: 'Select' }).props('options')).toContainEqual({ value: 9, label: '#9' })
    wrapper.unmount()
  })
})
