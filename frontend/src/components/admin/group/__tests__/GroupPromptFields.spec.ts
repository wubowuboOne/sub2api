import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createI18n } from 'vue-i18n'
import GroupPromptFields from '../GroupPromptFields.vue'
import { getGroupPrompt, saveGroupPrompt } from '@/api/admin/groupPrompt'

vi.mock('@/api/admin/groupPrompt', () => ({ getGroupPrompt: vi.fn(), saveGroupPrompt: vi.fn() }))

function mountFields() {
  return mount(GroupPromptFields, {
    props: { groupId: 7 },
    global: { plugins: [createI18n({ legacy: false, locale: 'zh', messages: {} })] },
  })
}

describe('group prepend prompt', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.mocked(getGroupPrompt).mockResolvedValue({ enabled: false, content: '', default_content: 'bundled default' })
    vi.mocked(saveGroupPrompt).mockResolvedValue(undefined)
  })

  it('starts disabled and persists bundled content with the group save', async () => {
    const wrapper = mountFields()
    await flushPromises()
    expect(wrapper.find('textarea').exists()).toBe(false)
    await wrapper.get('[role=switch]').trigger('click')
    expect((wrapper.get('textarea').element as HTMLTextAreaElement).value).toBe('bundled default')
    await wrapper.vm.save()
    expect(saveGroupPrompt).toHaveBeenCalledWith(7, { enabled: true, content: '' })
    await wrapper.vm.save()
    expect(saveGroupPrompt).toHaveBeenCalledTimes(1)
  })

  it('keeps custom content while disabling and supports restoring defaults', async () => {
    vi.mocked(getGroupPrompt).mockResolvedValue({ enabled: true, content: 'custom', default_content: 'bundled default' })
    const wrapper = mountFields()
    await flushPromises()
    await wrapper.get('textarea').setValue('edited')
    await wrapper.get('[role=switch]').trigger('click')
    await wrapper.vm.save()
    expect(saveGroupPrompt).toHaveBeenLastCalledWith(7, { enabled: false, content: 'edited' })
    await wrapper.get('[role=switch]').trigger('click')
    await wrapper.get('button.btn-secondary').trigger('click')
    await wrapper.vm.save()
    expect(saveGroupPrompt).toHaveBeenLastCalledWith(7, { enabled: true, content: '' })
  })

  it('preserves prompt settings after failed loading without blocking the group save', async () => {
    vi.mocked(getGroupPrompt).mockRejectedValueOnce(new Error('offline'))
    const wrapper = mountFields()
    await flushPromises()
    expect(wrapper.find('[role=alert]').exists()).toBe(true)
    await expect(wrapper.vm.save()).resolves.toBeUndefined()
    expect(saveGroupPrompt).not.toHaveBeenCalled()
    await wrapper.get('[role=alert] button').trigger('click')
    await flushPromises()
    expect(wrapper.find('[role=alert]').exists()).toBe(false)
  })

  it('does not wait for prompt loading or save unchanged settings', async () => {
    let resolve!: (value: Awaited<ReturnType<typeof getGroupPrompt>>) => void
    vi.mocked(getGroupPrompt).mockReturnValueOnce(new Promise(done => { resolve = done }))
    const wrapper = mountFields()
    await wrapper.vm.save()
    expect(saveGroupPrompt).not.toHaveBeenCalled()
    resolve({ enabled: false, content: '', default_content: 'bundled default' })
    await flushPromises()
    await wrapper.vm.save()
    expect(saveGroupPrompt).not.toHaveBeenCalled()
  })

  it('propagates save failures so the editor stays open', async () => {
    const wrapper = mountFields()
    await flushPromises()
    await wrapper.get('[role=switch]').trigger('click')
    vi.mocked(saveGroupPrompt).mockRejectedValueOnce(new Error('offline'))
    await expect(wrapper.vm.save()).rejects.toThrow('offline')
    await wrapper.vm.save()
    expect(saveGroupPrompt).toHaveBeenCalledTimes(2)
  })

  it('loads and saves independent model prompts with trimmed names', async () => {
    vi.mocked(getGroupPrompt).mockResolvedValue({
      enabled: true, content: 'group fallback', default_content: 'bundled default',
      model_prompts: { 'model-a': 'prompt a' },
    })
    const wrapper = mountFields()
    await flushPromises()
    expect(wrapper.findAll('[data-testid=model-prompt-row]')).toHaveLength(1)
    expect((wrapper.get('[data-testid=model-prompt-content]').element as HTMLTextAreaElement).value).toBe('prompt a')
    await wrapper.get('[data-testid=add-model-prompt]').trigger('click')
    await wrapper.findAll('[data-testid=model-prompt-name]')[1]!.setValue(' model-b ')
    await wrapper.findAll('[data-testid=model-prompt-content]')[1]!.setValue('prompt b')
    await wrapper.vm.save()
    expect(saveGroupPrompt).toHaveBeenLastCalledWith(7, {
      enabled: true, content: 'group fallback', model_prompts: { 'model-a': 'prompt a', 'model-b': 'prompt b' },
    })
  })

  it('saves an empty model prompt as bundled default and removes old rules', async () => {
    const wrapper = mountFields()
    await flushPromises()
    await wrapper.get('[role=switch]').trigger('click')
    await wrapper.get('[data-testid=add-model-prompt]').trigger('click')
    await wrapper.get('[data-testid=model-prompt-name]').setValue('model-a')
    await wrapper.vm.save()
    expect(saveGroupPrompt).toHaveBeenLastCalledWith(7, {
      enabled: true, content: '', model_prompts: { 'model-a': '' },
    })
    await wrapper.get('[data-testid=remove-model-prompt]').trigger('click')
    await wrapper.vm.save()
    expect(saveGroupPrompt).toHaveBeenLastCalledWith(7, { enabled: true, content: '' })
  })

  it('blocks blank and duplicate model names before saving', async () => {
    const wrapper = mountFields()
    await flushPromises()
    await wrapper.get('[role=switch]').trigger('click')
    await wrapper.get('[data-testid=add-model-prompt]').trigger('click')
    await expect(wrapper.vm.save()).rejects.toThrow('完整模型名')
    await wrapper.get('[data-testid=model-prompt-name]').setValue('model-a')
    await wrapper.get('[data-testid=add-model-prompt]').trigger('click')
    await wrapper.findAll('[data-testid=model-prompt-name]')[1]!.setValue(' model-a ')
    await expect(wrapper.vm.save()).rejects.toThrow('不能重复')
    expect(saveGroupPrompt).not.toHaveBeenCalled()
  })
})
