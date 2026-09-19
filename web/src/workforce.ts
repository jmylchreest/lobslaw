import { ApiError, readSSE, request } from './api';

export interface Project {
  id: string; owner: string; name: string; description: string; coordinator_bot_id: string;
  bot_ids: string[]; context: string; status: 'active' | 'archived'; revision: number;
}
export interface ProjectMessage {
  id: string; project_id: string; role: string; speaker_id: string; speaker_name?: string;
  content: string; task_id?: string; created_at: string;
}
export type TaskStatus = 'planned' | 'ready' | 'running' | 'blocked' | 'needs_approval' | 'done' | 'failed' | 'cancelled';
export interface Artifact { id: string; name: string; kind: string; reference: string }
export interface Task {
  id: string; project_id: string; owner: string; title: string; instructions: string;
  assignee_bot_id: string; acceptance_criteria: string[]; depends_on: string[]; status: TaskStatus;
  revision: number; checkpoint?: number; result?: string; error?: string; question?: string;
  prompt_id?: string; artifacts: Artifact[]; created_at: string; updated_at: string;
}
export interface AttentionItem {
  id: string; kind: 'approval' | 'blocked' | 'failed' | 'deliverable' | 'takeover';
  title: string; detail: string; project_id?: string; task_id?: string; prompt_id?: string; created_at: string;
}
export interface RoutineStep {
  action: 'navigate' | 'click' | 'fill' | 'press' | 'wait' | 'capture';
  selector?: string; value?: string; url?: string; description?: string; sensitive?: boolean;
}
export interface Workflow {
  id: string; project_id: string; name: string; description: string; instructions: string;
  steps: RoutineStep[]; status: 'draft' | 'approved' | 'disabled'; revision: number; approved_digest?: string;
  schedule?: string;
}
export interface Trigger {
  id: string; project_id: string; name: string; routine_id?: string; instructions?: string;
  assignee_bot_id?: string; enabled: boolean; revision: number; last_fired_at?: string;
}
export interface ComputerState {
  project_id: string; available: boolean; control: 'human' | 'bot'; recording: boolean; steps: RoutineStep[];
}
const id = encodeURIComponent;
const post = (body: unknown): RequestInit => ({ method: 'POST', body: JSON.stringify(body) });
const patch = (body: unknown): RequestInit => ({ method: 'PATCH', body: JSON.stringify(body) });
export const workforce = {
  projects: () => request<{ projects: Project[] }>('/v1/projects').then(r => r.projects ?? []),
  project: (key: string) => request<Project>(`/v1/projects/${id(key)}`),
  createProject: (body: Partial<Project>) => request<Project>('/v1/projects', post(body)),
  updateProject: (key: string, body: Partial<Project>) => request<Project>(`/v1/projects/${id(key)}`, patch(body)),
  messages: (key: string) => request<{ messages: ProjectMessage[] }>(`/v1/projects/${id(key)}/messages`).then(r => r.messages ?? []),
  tasks: (key: string) => request<{ tasks: Task[] }>(`/v1/projects/${id(key)}/tasks`).then(r => r.tasks ?? []),
  task: (key: string) => request<Task>(`/v1/tasks/${id(key)}`),
  createTask: (key: string, body: Partial<Task>) => request<Task>(`/v1/projects/${id(key)}/tasks`, post(body)),
  actTask: (task: Task, action: string, answer?: string) => request<Task>(`/v1/tasks/${id(task.id)}`, patch({ revision: task.revision, action, answer })),
  attention: () => request<{ items: AttentionItem[] }>('/v1/attention').then(r => r.items ?? []),
  routines: (key: string) => request<{ routines: Workflow[] }>(`/v1/projects/${id(key)}/routines`).then(r => r.routines ?? []),
  draft: (key: string, body: Partial<Workflow>) => request<Workflow>(`/v1/projects/${id(key)}/routines`, post(body)),
  editRoutine: (routine: Workflow, action: string, changes?: Partial<Workflow>) => request<Workflow>(`/v1/routines/${id(routine.id)}`, patch({ ...changes, revision: routine.revision, action })),
  runRoutine: (key: string) => request<Task>(`/v1/routines/${id(key)}/run`, post({})),
  triggers: (key: string) => request<{ triggers: Trigger[] }>(`/v1/projects/${id(key)}/triggers`).then(r => r.triggers ?? []),
  createTrigger: (key: string, body: Partial<Trigger>) => request<Trigger>(`/v1/projects/${id(key)}/triggers`, post(body)),
  fireTrigger: (key: string, event_id: string) => request<{ task: Task; duplicate: boolean }>(`/v1/triggers/${id(key)}/fire`, post({ event_id })),
  computer: (key: string) => request<ComputerState>(`/v1/computers/${id(key)}`),
  computerAction: (key: string, body: { action: string; selector?: string; value?: string; url?: string; sensitive?: boolean; x?:number; y?:number }) => request<ComputerState>(`/v1/computers/${id(key)}`, post(body)),
};

export async function projectChat(project: string, message: string, bot_id: string | undefined,
  onEvent: (event: string, data: Record<string, unknown>) => void, signal?: AbortSignal) {
  const res = await fetch(`/v1/projects/${id(project)}/messages`, {
    ...post({ message, bot_id }), credentials: 'include', signal,
    headers: { 'Content-Type': 'application/json', Accept: 'text/event-stream' },
  });
  if (!res.ok) throw new ApiError(res.status, await res.text());
  await readSSE(res, onEvent);
}

// Deliverable references must stay on an authenticated API surface. A model's
// arbitrary URL/path is rendered as text, never as a privileged download link.
export function artifactURL(task: Task, artifact: Artifact): string | undefined {
  const result = `/v1/tasks/${id(task.id)}/result`;
  if (artifact.reference === result) return result;
  const prefix = `/v1/tasks/${id(task.id)}/artifacts/`;
  return artifact.reference.startsWith(prefix) && !artifact.reference.includes('..') ? artifact.reference : undefined;
}
