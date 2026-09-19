import { afterEach, describe, expect, it, vi } from 'vitest';
import { workforce, artifactURL, type Task } from './workforce';

afterEach(() => vi.unstubAllGlobals());
describe('workforce request boundaries', () => {
  it('sends revision-fenced task approval, never a generic prompt approval', async () => {
    const fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify({ id: 'task', revision: 5 })));
    vi.stubGlobal('fetch', fetch);
    await workforce.actTask({ id: 'task', revision: 4 } as Task, 'approve');
    expect(fetch.mock.calls[0][0]).toBe('/v1/tasks/task');
    expect(JSON.parse(fetch.mock.calls[0][1].body)).toEqual({ revision: 4, action: 'approve' });
  });
  it('does not turn model-authored filesystem paths or remote links into downloads', () => {
    const task = { id: 'task' } as Task;
    for (const reference of ['file:///etc/passwd', 'https://evil.test/', '/v1/tasks/other/artifacts/key', '/v1/tasks/task/artifacts/../secret']) {
      expect(artifactURL(task, { id: 'a', name: 'a', kind: 'file', reference })).toBeUndefined();
    }
    expect(artifactURL(task, { id: 'a', name: 'a', kind: 'file', reference: '/v1/tasks/task/artifacts/a' })).toBe('/v1/tasks/task/artifacts/a');
  });
});
