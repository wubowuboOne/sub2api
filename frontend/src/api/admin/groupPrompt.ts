import { apiClient } from '../client'

export interface GroupPromptConfig {
  enabled: boolean
  content: string
  model_prompts?: Record<string, string> | null
  default_content: string
}

export async function getGroupPrompt(id: number): Promise<GroupPromptConfig> {
  const { data } = await apiClient.get<GroupPromptConfig>(`/admin/groups/${id}/prepend-prompt`)
  return data
}

export async function saveGroupPrompt(id: number, config: Pick<GroupPromptConfig, 'enabled' | 'content' | 'model_prompts'>): Promise<void> {
  await apiClient.put(`/admin/groups/${id}/prepend-prompt`, config)
}
