import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import OpenAIWebSearchSettings from '../OpenAIWebSearchSettings.vue'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('../AccountTestModal.vue', () => ({ default: { props: ['show', 'account', 'initialMode'], template: '<div v-if="show" data-testid="probe-modal">{{ initialMode }}</div>' } }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { getById: vi.fn() } } }))
vi.mock('@/utils/format', () => ({ formatDateTime: (value: string) => value }))

describe('OpenAIWebSearchSettings', () => {
  it('switches exclusively between search-only, disabled, and normal operation', async () => {
    const wrapper = mount(OpenAIWebSearchSettings, { props: { modelValue: 'default' } })
    const switches = wrapper.findAll('[role="switch"]')
    expect(switches).toHaveLength(2)
    await switches[0]!.trigger('click')
    expect(wrapper.emitted('update:modelValue')?.at(-1)).toEqual(['only'])
    await wrapper.setProps({ modelValue: 'only' })
    expect(switches[0]!.attributes('aria-checked')).toBe('true')
    expect(switches[1]!.attributes('aria-checked')).toBe('false')
    await switches[1]!.trigger('click')
    expect(wrapper.emitted('update:modelValue')?.at(-1)).toEqual(['disabled'])
    await wrapper.setProps({ modelValue: 'disabled' })
    expect(switches[0]!.attributes('aria-checked')).toBe('false')
    await switches[1]!.trigger('click')
    expect(wrapper.emitted('update:modelValue')?.at(-1)).toEqual(['default'])
  })
  it('opens the explicit search probe without changing scheduling mode', async () => {
    const wrapper = mount(OpenAIWebSearchSettings, { props: { modelValue: 'disabled', account: { id: 7, platform: 'openai', type: 'apikey' } as never } })
    await wrapper.find('button.btn').trigger('click')
    expect(wrapper.find('[data-testid="probe-modal"]').text()).toBe('web_search')
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
  })
})
