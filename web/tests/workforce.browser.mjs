// Browser-driven consumer-contract QA. API fixtures exist only in this test;
// real Chromium/egress/profile behavior is exercised by internal/computer.
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import { createServer } from 'vite';

const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const server = await createServer({ server: { host: '127.0.0.1', port: 0 }, logLevel: 'error' });
await server.listen();
const address = server.httpServer.address();
const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH, headless: true });
const requests = [];
const bot = { id: 'chief', display_name: 'Chief', enabled: true, tools: [], group_id: 'team', revision: 1 };
const project = { id: 'project', name: 'Launch the research', description: 'A clear result', owner: 'user:alice', bot_ids: ['chief'], coordinator_bot_id: 'chief', context: '', revision: 1, status: 'active' };
const task = { id: 'task', project_id: 'project', title: 'Find the sources', instructions: 'Research the source material', assignee_bot_id: 'chief', status: 'planned', revision: 1, depends_on: [], acceptance_criteria: ['Cite evidence'], artifacts: [] };
let projects = [], tasks = [], routines = [], triggers = [];
const computer = { project_id: 'project', available: false, control: 'bot', recording: false, steps: [] };
let conflict = false, chatApproval = false, chatQuestion = false;
try {
  const page = await browser.newPage({ viewport: { width: 1365, height: 900 } });
  const pageErrors = [];
  page.on('pageerror', error => pageErrors.push(error.message));
  await page.route('**/v1/**', async route => {
    const req = route.request(), path = new URL(req.url()).pathname, method = req.method();
    const body = method === 'GET' ? undefined : req.postDataJSON();
    requests.push({ path, method, body });
    const json = (data, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(data) });
    if (path === '/v1/session') return json({ user_id: 'alice' });
    if (path === '/v1/capabilities') return json(Object.fromEntries(['compute', 'compute-teams', 'ui-web'].map(k => [k, { enabled: true, authorised: true, configured: true, available: true }])));
    if (path === '/v1/bots') return json({ bots: [bot] });
    if (path === '/v1/groups') return json({ groups: [] });
    if (path === '/v1/activity') return json({ items: [] });
    if (path === '/v1/projects' && method === 'POST') { projects = [project]; return json(project, 201); }
    if (path === '/v1/projects') return json({ projects });
    if (path === '/v1/projects/project') return json(project);
    if (path === '/v1/projects/project/messages' && method === 'POST') {
      if (chatQuestion) {
        tasks[0] = {...tasks[0],status:'blocked',question:'Which account should I use?',manual_step:false};
        return route.fulfill({contentType:'text/event-stream',body:'event: blocked\ndata: {"task_id":"task","reason":"Which account should I use?"}\n\n'});
      }
      if (chatApproval) {
        tasks[0] = { ...tasks[0], status:'needs_approval', question:'Approve the project action', prompt_id:'opaque-task-approval' };
        return route.fulfill({ contentType:'text/event-stream', body:'event: needs_confirmation\ndata: {"task_id":"task","prompt_id":"opaque-task-approval","reason":"Approve the project action"}\n\n' });
      }
      return route.fulfill({ contentType: 'text/event-stream', body: 'event: reply\ndata: {"reply":"The research is underway."}\n\n' });
    }
    if (path === '/v1/projects/project/messages') return json({ messages: [] });
    if (path === '/v1/projects/project/tasks' && method === 'POST') { tasks = [{ ...task, ...body }]; return json(tasks[0], 201); }
    if (path === '/v1/projects/project/tasks') return json({ tasks });
    if (path === '/v1/tasks/task' && method === 'PATCH') {
      if (conflict) return json({ error: 'stale task revision' }, 409);
      assert.equal(body.revision, tasks[0].revision);
      tasks[0] = { ...tasks[0], revision: tasks[0].revision + 1, acknowledged:body.action==='acknowledge', status: body.action==='acknowledge'?tasks[0].status:body.action === 'approve' ? 'ready' : 'running' };
      return json(tasks[0]);
    }
    if (path === '/v1/tasks/task') return json(tasks[0]);
    if (path === '/v1/attention') return json({ items: tasks[0]?.acknowledged ? [] : [{ id: 'approval', kind: 'approval', title: 'Review research action', detail: 'Navigate to the research source', task_id: 'task', project_id: 'project' }] });
    if (path === '/v1/computers/project/screenshot') return route.fulfill({ contentType: 'image/png', body: Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aV1cAAAAASUVORK5CYII=', 'base64') });
    if (path === '/v1/computers/project') {
      if (method === 'POST') {
        if (body.action === 'start') computer.available = true;
        if (body.action === 'takeover') computer.control = 'human';
        if (body.action === 'release') { computer.control = 'bot'; computer.recording = false; }
        if (body.action === 'record') { computer.recording = true; computer.steps = []; }
        if (body.action === 'fill' && computer.recording) computer.steps.push({ action: 'fill', sensitive: true, description: 'Enter manually' });
        if (body.action === 'navigate' && computer.recording) computer.steps.push(body);
      }
      return json(computer);
    }
    if (path === '/v1/projects/project/routines' && method === 'POST') {
      const routine = { ...body, id: 'routine', project_id: 'project', status: 'draft', revision: 1 }; routines.push(routine); return json(routine, 201);
    }
    if (path === '/v1/projects/project/routines') return json({ routines });
    if (path === '/v1/routines/routine' && method === 'PATCH') {
      assert.equal(body.revision, routines[0].revision);
      routines[0] = { ...routines[0], ...body, revision: routines[0].revision + 1, status: body.action === 'approve' ? 'approved' : 'draft' }; return json(routines[0]);
    }
    if (path === '/v1/projects/project/triggers' && method === 'POST') { triggers.push({ ...body, id: 'trigger', revision: 1 }); return json(triggers[0], 201); }
    if (path === '/v1/projects/project/triggers') return json({ triggers });
    return json({ error: `Unavailable fixture: ${method} ${path}` }, 503);
  });
  const base = `http://127.0.0.1:${address.port}`;
  await page.goto(`${base}/projects`);
  await page.getByText('Your next project starts here').waitFor();
  await page.getByRole('button', { name: '+ New project' }).click();
  await page.getByLabel('Project name', { exact: true }).fill(project.name);
  await page.getByLabel('What are you trying to achieve?').fill(project.description);
  await page.getByLabel('Coordinator', { exact: true }).selectOption('chief');
  await page.getByRole('button', { name: 'Create project', exact: true }).click();
  await page.getByLabel('Message the project').fill('Start with credible sources');
  await page.getByRole('button', { name: 'Send to project' }).click();
  await page.getByText('The research is underway.').waitFor();
  await page.getByRole('link', { name: 'tasks', exact: true }).click();
  await page.getByRole('button', { name: '+ Delegate task' }).click();
  await page.getByLabel('Task title').fill(task.title);
  await page.getByLabel('Instructions', { exact: true }).fill(task.instructions);
  await page.getByLabel('Assignee').selectOption('chief');
  await page.getByRole('button', { name: 'Create task', exact: true }).click();
  await page.getByRole('link', { name: /Find the sources/ }).click();
  conflict = true;
  await page.getByRole('button', { name: 'Start task' }).click();
  await page.getByText('stale task revision').waitFor();
  conflict = false;
  await page.getByRole('button', { name: 'Start task' }).click();
  await page.getByText('running', { exact: true }).waitFor();
  await page.goto(`${base}/projects/project/computer`);
  await page.getByRole('button', { name: 'Open browser', exact: true }).click();
  await page.getByRole('img', { name: 'Current project browser screenshot' }).waitFor();
  await page.getByRole('button', { name: 'Take control', exact: true }).click();
  await page.getByText('You have control · bot paused').waitFor();
  await page.getByRole('button', { name: 'Start new recording' }).click();
  await page.getByLabel('Website URL').fill('https://example.com/');
  await page.getByRole('button', { name: 'Go to website' }).click();
  await page.getByLabel('Element selector').fill('#password');
  await page.getByLabel('Private text entry (never recorded)').fill('fixture-secret');
  await page.getByRole('button', { name: 'Enter text' }).click();
  await page.getByText('Manual entry required — no value saved').waitFor();
  await page.getByLabel('Routine name', { exact: true }).fill('Research demonstration');
  await page.getByRole('button', { name: 'Save routine draft', exact: true }).click();
  await page.getByText('Draft saved.').waitFor();
  const draftRequest = requests.find(r => r.path.endsWith('/routines') && r.method === 'POST');
  assert(!JSON.stringify(draftRequest.body).includes('fixture-secret'));
  assert.equal(draftRequest.body.steps[1].sensitive, true);
  await page.getByRole('link', { name: /Review and explicitly approve/ }).click();
  await page.getByLabel('I reviewed this exact definition and its browser actions.').check();
  await page.getByRole('button', { name: 'Approve definition', exact: true }).click();
  await page.getByRole('button', { name: 'Run as task', exact: true }).waitFor();
  await page.getByRole('button', { name: 'Edit definition', exact: true }).click();
  await page.getByLabel('Fill step', { exact:true }).selectOption('1');
  await page.getByLabel('Replay target selector').fill('#query');
  await page.getByLabel('Non-sensitive replay value').fill('red pandas');
  assert.equal(await page.getByRole('button',{name:'Use reviewed input in draft'}).isDisabled(),true);
  await page.getByLabel('This value is not a password, login credential, token, or other secret.').check();
  await page.getByRole('button',{name:'Use reviewed input in draft'}).click();
  await page.getByLabel('Routine instructions').fill('An edited definition');
  await page.getByLabel('Schedule (cron, optional)').fill('0 9 * * 1-5');
  await page.getByRole('button', { name: 'Save changes as draft' }).click();
  await page.getByRole('button', { name: 'Approve definition', exact: true }).waitFor();
  assert.equal(await page.getByRole('button', { name: 'Run as task', exact: true }).count(), 0);
  assert.equal(routines[0].status, 'draft');
  assert.equal(routines[0].steps[1].input_mode,'reviewed_literal');
  assert.equal(routines[0].steps[1].value,'red pandas');
  assert.equal(routines[0].steps[1].sensitive,false);
  assert(!JSON.stringify(routines).includes('fixture-secret'));
  tasks[0] = { ...tasks[0], status: 'needs_approval', question: 'Navigate to the research source', prompt_id: 'task:approval' };
  await page.goto(`${base}/attention`);
  await page.getByRole('link', { name: 'Review task approval →' }).click();
  await page.getByRole('button', { name: 'Approve this task action' }).click();
  await page.getByText('ready', { exact: true }).waitFor();
  chatApproval = true;
  await page.goto(`${base}/projects/project/channel`);
  await page.getByLabel('Message the project').fill('Please perform this action');
  await page.getByRole('button', {name:'Send to project'}).click();
  await page.getByRole('button', {name:'Approve this task action'}).click();
  await page.getByRole('heading', {name:'Needs your approval'}).waitFor({state:'hidden'});
  assert.equal(requests.findLast(r=>r.method==='PATCH' && r.path==='/v1/tasks/task').body.action,'approve');
  assert.equal(requests.filter(r => r.path.startsWith('/v1/prompts/')).length, 0);
  chatQuestion = true;
  await page.getByLabel('Message the project').fill('Continue the research');
  await page.getByRole('button', {name:'Send to project'}).click();
  await page.getByLabel('Reply to the teammate').fill('Use the staging account');
  await page.getByRole('button', {name:'Answer and resume'}).click();
  await page.getByRole('heading', {name:'Needs your input'}).waitFor({state:'hidden'});
  assert.equal(requests.findLast(r=>r.method==='PATCH' && r.path==='/v1/tasks/task').body.answer,'Use the staging account');
  tasks[0] = { ...tasks[0], status:'blocked', manual_step:false };
  await page.goto(`${base}/tasks/task`);
  await page.getByText('blocked', {exact:true}).waitFor();
  assert.equal(await page.getByRole('button', {name:'I completed this manual browser step'}).count(),0);
  tasks[0] = { ...tasks[0], manual_step:true };
  await page.getByRole('button', {name:'Reload latest revision'}).click();
  await page.getByRole('button', {name:'I completed this manual browser step'}).waitFor();
  tasks[0] = { ...tasks[0], status:'done', result:'Report delivered' };
  await page.getByRole('button', {name:'Reload latest revision'}).click();
  await page.getByRole('button', {name:'Mark reviewed in Attention'}).click();
  await page.getByRole('button', {name:'Mark reviewed in Attention'}).waitFor({state:'hidden'});
  await page.goto(`${base}/attention`);
  await page.getByText("You're all caught up").waitFor();
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`${base}/projects/project/tasks`);
  await page.getByRole('heading', { name: 'Task board' }).waitFor();
  assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'mobile view overflows horizontally');
  await page.getByRole('button', { name: 'Open menu' }).click();
  await page.getByRole('link', { name: 'Attention', exact: true }).click();
  await page.getByRole('heading', { name: 'Attention', exact: true }).waitFor();
  assert.deepEqual(pageErrors, []);
  console.info('PASS: desktop/mobile project → channel → tasks/conflict → computer/takeover → secret-free draft → approval/edit invalidation → Attention task approval');
} finally {
  await browser.close(); await server.close();
}
