import { cloneElement, useEffect, useId, useRef, useState, type FormEvent, type ReactNode, type ReactElement } from 'react';
import { Link, NavLink, useNavigate, useParams } from 'react-router-dom';
import { api, type Bot } from '../api';
import { Markdown as RichText } from '../components/Markdown';
import { Empty, Err, Spinner, useLoad, when } from '../components/ui';
import { Approval as PromptApproval } from './BotRoom';
import { Computer } from './WorkforceComputer';
import { Workflows } from './WorkforceRoutines';
import { artifactURL, projectChat, workforce, type Project, type TaskStatus } from '../workforce';
import './workforce.css';

const refreshMillis = 5000;
function Markdown({ text }: { text: string }) { return <RichText>{text}</RichText>; }
export function useWork<T>(fn: () => Promise<T>, deps: unknown[] = []) {
  const load = useLoad(fn, deps);
  useEffect(() => { const timer = setInterval(load.reload, refreshMillis); return () => clearInterval(timer); }, [load.reload]);
  return load;
}
export function useAction() {
  const [busy, setBusy] = useState(false), [error, setError] = useState<Error | null>(null);
  const run = async (fn: () => Promise<unknown>) => {
    setBusy(true); setError(null);
    try { await fn(); } catch (e) { setError(e instanceof Error ? e : new Error(String(e))); }
    finally { setBusy(false); }
  };
  return { busy, error, run };
}
export function WorkPage({ title, sub, children }: { title: ReactNode; sub?: ReactNode; children: ReactNode }) {
  return <section className="wf"><header className="wf-heading"><h1>{title}</h1>{sub && <p>{sub}</p>}</header>{children}</section>;
}
export function Field({ label, children }: { label: string; children: ReactElement<{id?:string}> }) {
  const id=useId();
  return <div className="wf-field"><label htmlFor={id}>{label}</label>{cloneElement(children,{id})}</div>;
}
export function BotSelect({ bots, value, change, optional = false, id }: { bots: Bot[]; value: string; change: (value: string) => void; optional?: boolean; id?:string }) {
  return <select id={id} required={!optional} value={value} onChange={e => change(e.target.value)}><option value="">{optional ? 'Project coordinator' : 'Select a teammate'}</option>{bots.map(b => <option key={b.id} value={b.id}>{b.display_name || b.id}</option>)}</select>;
}
export function Projects() {
  const projects = useWork(workforce.projects), bots = useLoad(api.listBots), action = useAction(), navigate = useNavigate();
  const [creating, setCreating] = useState(false), [name, setName] = useState(''), [description, setDescription] = useState(''), [coordinator, setCoordinator] = useState(''), [roster, setRoster] = useState<string[]>([]);
  return <WorkPage title="Projects" sub="A shared brief, a working team, and a clear outcome.">
    <div className="wf-toolbar"><button onClick={() => setCreating(!creating)}>{creating ? 'Close form' : '+ New project'}</button><Link to="/attention">Attention inbox →</Link></div>
    {projects.error && <Err error={projects.error} />}
    {creating && <form className="wf-panel wf-form" onSubmit={e => { e.preventDefault(); void action.run(async () => {
      const p = await workforce.createProject({ name, description, coordinator_bot_id: coordinator, bot_ids: [...new Set([...roster, coordinator])], context: '' }); navigate(`/projects/${p.id}/channel`);
    }); }}>
      <h2>Bring a team around an outcome</h2>
      <Field label="Project name"><input required value={name} onChange={e => setName(e.target.value)} /></Field>
      <Field label="What are you trying to achieve?"><textarea required value={description} onChange={e => setDescription(e.target.value)} /></Field>
      {bots.error && <Err error={bots.error} />}
      <Field label="Coordinator"><BotSelect bots={bots.data ?? []} value={coordinator} change={setCoordinator} /></Field>
      <fieldset><legend>Project teammates</legend>{(bots.data ?? []).map(b => <label className="wf-check" key={b.id}><input type="checkbox" checked={roster.includes(b.id)} onChange={e => setRoster(e.target.checked ? [...roster, b.id] : roster.filter(id => id !== b.id))} />{b.display_name || b.id}</label>)}</fieldset>
      {action.error && <Err error={action.error} />}<button disabled={action.busy || !coordinator}>Create project</button>
    </form>}
    {!projects.data && projects.loading && <Spinner />}
    {projects.data?.length === 0 && <Empty title="Your next project starts here" hint="Create a project, choose its coordinator, and describe the outcome in the shared channel." />}
    <div className="wf-grid">{projects.data?.map(p => <Link className="wf-panel wf-project" key={p.id} to={`/projects/${p.id}/channel`}><small>{p.status} · {p.bot_ids?.length ?? 0} teammates</small><h2>{p.name}</h2><p>{p.description}</p><span>Open workspace →</span></Link>)}</div>
  </WorkPage>;
}

export function ProjectRoom() {
  const { projectId = '', tab = 'channel' } = useParams();
  const project = useWork(() => workforce.project(projectId), [projectId]);
  if (!project.data || project.data.id!==projectId) return <WorkPage title="Project">{project.error ? <Err error={project.error} /> : <Spinner />}</WorkPage>;
  const p = project.data;
  return <WorkPage title={p.name} sub={p.description}>
    <Link className="wf-back" to="/projects">← All projects</Link>
    <nav className="wf-tabs" aria-label="Project workspace">{['channel', 'tasks', 'routines', 'computer', 'brief'].map(name => <NavLink key={name} to={`/projects/${projectId}/${name}`}>{name}</NavLink>)}</nav>
    {project.error && <Err error={project.error} />}
    <div key={`${projectId}:${tab}`}>
      {tab === 'channel' ? <ProjectChannel project={p} /> : tab === 'tasks' ? <TaskBoard project={p} /> : tab === 'routines' ? <Workflows project={p} /> : tab === 'computer' ? <Computer project={p} /> : tab === 'brief' ? <ProjectBrief project={p} changed={project.reload} /> : <Empty title="Unknown project view" />}
    </div>
  </WorkPage>;
}

function ProjectBrief({ project, changed }: { project: Project; changed: () => void }) {
  const [context, setContext] = useState(project.context), [revision,setRevision]=useState(project.revision), action = useAction();
  return <form className="wf-panel wf-form" onSubmit={e => { e.preventDefault(); void action.run(async () => { const saved=await workforce.updateProject(project.id, { revision, context });setRevision(saved.revision); changed(); }); }}><h2>Shared context</h2><p>Persistent project guidance available to the team. Keep credentials in the browser's private profile.</p><Field label="Project brief"><textarea rows={10} value={context} onChange={e => setContext(e.target.value)} /></Field>{action.error && <Err error={action.error} />}<div className="wf-toolbar"><button disabled={action.busy}>Save brief</button><button type="button" onClick={()=>{setContext(project.context);setRevision(project.revision);}}>Load latest brief</button></div></form>;
}

function ProjectChannel({ project }: { project: Project }) {
  const messages = useWork(() => workforce.messages(project.id), [project.id]), bots = useLoad(api.listBots);
  const action = useAction(), [text, setText] = useState(''), [bot, setBot] = useState(''), [reply, setReply] = useState(''), [prompt, setPrompt] = useState<{taskID:string;reason:string} | null>(null);
  const controller = useRef<AbortController | null>(null);
  useEffect(() => () => controller.current?.abort(), []);
  function send(e: FormEvent) {
    e.preventDefault(); if (!text.trim()) return;
    void action.run(async () => {
      controller.current = new AbortController(); setReply(''); setPrompt(null);
      await projectChat(project.id, text, bot || undefined, (event, data) => {
        if (event === 'reply') setReply(String(data.reply ?? data.text ?? ''));
        if (event === 'needs_confirmation' || event === 'blocked') setPrompt({taskID:String(data.task_id ?? ''),reason:String(data.reason ?? data.confirmation_reason ?? 'Your input is needed')});
      }, controller.current.signal);
      setText(''); messages.reload();
    });
  }
  return <div className="wf-channel">
    <p className="wf-muted">One shared conversation. {project.bot_ids?.length ?? 0} teammates can use its context; your selected bot answers.</p>
    {messages.error && <Err error={messages.error} />}{!messages.data && messages.loading && <Spinner />}
    {messages.data?.length === 0 && <Empty title="Give the team its first brief" hint="Discuss the goal here, then create trackable work in Tasks." />}
    <div className="wf-thread">{messages.data?.map(m => <article className={`wf-message ${m.role}`} key={m.id}><header><b>{m.speaker_name || m.speaker_id || m.role}</b><time>{when(m.created_at)}</time></header><Markdown text={m.content} />{m.task_id && <Link to={`/tasks/${m.task_id}`}>View task →</Link>}</article>)}</div>
    {reply && <div className="wf-panel"><Markdown text={reply} /></div>}
    {prompt?.taskID && <TaskApproval taskID={prompt.taskID} reason={prompt.reason} onAnswered={() => { setPrompt(null); messages.reload(); }} />}
    {action.error && <Err error={action.error} />}
    <form className="wf-panel wf-form" onSubmit={send}><Field label="Responding teammate"><BotSelect bots={(bots.data ?? []).filter(b => project.bot_ids?.includes(b.id))} value={bot} change={setBot} optional /></Field><Field label="Message the project"><textarea required value={text} onChange={e => setText(e.target.value)} placeholder="What should the team work towards?" /></Field><div className="wf-toolbar"><button disabled={action.busy}>{action.busy ? 'Working…' : 'Send to project'}</button>{action.busy && <button type="button" onClick={() => controller.current?.abort()}>Stop waiting</button>}</div></form>
  </div>;
}

function TaskApproval({taskID, reason, onAnswered}: {taskID:string; reason:string; onAnswered:()=>void}) {
  const task = useLoad(() => workforce.task(taskID), [taskID]), action = useAction();
  const [clarification,setClarification] = useState('');
  const answer = (decision:string) => void action.run(async () => {
    if (!task.data) return;
    await workforce.actTask(task.data, decision, decision==='answer'?clarification:undefined);
    onAnswered();
  });
  return <section className="wf-panel"><h3>{task.data?.status==='blocked'?'Needs your input':'Needs your approval'}</h3><Markdown text={reason} />
    {task.loading && <Spinner />}{task.error && <Err error={task.error} />}{action.error && <Err error={action.error} />}
    {task.data?.status === 'needs_approval' && <div className="wf-toolbar"><button disabled={action.busy} onClick={()=>answer('approve')}>Approve this task action</button><button disabled={action.busy} onClick={()=>answer('cancel')}>Deny and cancel task</button></div>}
    {task.data?.status==='blocked' && !task.data.manual_step && <form className="wf-form" onSubmit={e=>{e.preventDefault();answer('answer');}}><Field label="Reply to the teammate"><textarea required value={clarification} onChange={e=>setClarification(e.target.value)} /></Field><button disabled={action.busy}>Answer and resume</button></form>}
    <Link to={`/tasks/${taskID}`}>Review task →</Link>
  </section>;
}

const columns: { title: string; statuses: TaskStatus[] }[] = [
  { title: 'To do', statuses: ['planned', 'ready'] }, { title: 'In progress', statuses: ['running'] },
  { title: 'Needs attention', statuses: ['blocked', 'needs_approval', 'failed'] }, { title: 'Finished', statuses: ['done', 'cancelled'] },
];
function TaskBoard({ project }: { project: Project }) {
  const tasks = useWork(() => workforce.tasks(project.id), [project.id]), bots = useLoad(api.listBots), action = useAction();
  const [creating, setCreating] = useState(false), [title, setTitle] = useState(''), [instructions, setInstructions] = useState(''), [assignee, setAssignee] = useState(''), [criteria, setCriteria] = useState(''), [dependencies, setDependencies] = useState<string[]>([]);
  return <>
    <div className="wf-toolbar"><h2>Task board</h2><button onClick={() => setCreating(!creating)}>{creating ? 'Close form' : '+ Delegate task'}</button><button onClick={tasks.reload}>Refresh</button></div>
    {tasks.error && <Err error={tasks.error} />}
    {creating && <form className="wf-panel wf-form" onSubmit={e => { e.preventDefault(); void action.run(async () => {
      await workforce.createTask(project.id, { title, instructions, assignee_bot_id: assignee, acceptance_criteria: criteria.split('\n').filter(Boolean), depends_on: dependencies }); setCreating(false); tasks.reload();
    }); }}>
      <Field label="Task title"><input required value={title} onChange={e => setTitle(e.target.value)} /></Field>
      <Field label="Instructions"><textarea required value={instructions} onChange={e => setInstructions(e.target.value)} /></Field>
      <Field label="Assignee"><BotSelect bots={(bots.data ?? []).filter(b => project.bot_ids?.includes(b.id))} value={assignee} change={setAssignee} /></Field>
      <Field label="Acceptance criteria (one per line)"><textarea value={criteria} onChange={e => setCriteria(e.target.value)} /></Field>
      <fieldset><legend>Wait for these tasks</legend>{tasks.data?.map(t => <label className="wf-check" key={t.id}><input type="checkbox" checked={dependencies.includes(t.id)} onChange={e => setDependencies(e.target.checked ? [...dependencies, t.id] : dependencies.filter(id => id !== t.id))} />{t.title} ({t.status})</label>)}</fieldset>
      {action.error && <Err error={action.error} />}<button disabled={action.busy}>Create task</button>
    </form>}
    {!tasks.data && tasks.loading && <Spinner />}
    {tasks.data?.length === 0 && <Empty title="No delegated work yet" hint="Give a teammate an outcome and optional dependencies. Work starts automatically when its prerequisites are ready." />}
    <div className="wf-board">{columns.map(column => <section key={column.title} className="wf-column"><h3>{column.title}<span>{tasks.data?.filter(t => column.statuses.includes(t.status)).length ?? 0}</span></h3>{tasks.data?.filter(t => column.statuses.includes(t.status)).map(t => <Link className="wf-task" to={`/tasks/${t.id}`} key={t.id}><span className={`wf-status ${t.status}`}>{t.status.replace('_', ' ')}</span><h4>{t.title}</h4><p>{bots.data?.find(b => b.id === t.assignee_bot_id)?.display_name || t.assignee_bot_id}</p>{!!t.depends_on?.length && <small>↳ {t.depends_on.length} dependencies</small>}{t.question && <p>{t.question}</p>}</Link>)}</section>)}</div>
  </>;
}

export function TaskDetails() {
  const { taskId = '' } = useParams(), task = useWork(() => workforce.task(taskId), [taskId]), action = useAction();
  const [answer, setAnswer] = useState('');
  if (!task.data || task.data.id!==taskId) return <WorkPage title="Task">{task.error ? <Err error={task.error} /> : <Spinner />}</WorkPage>;
  const t = task.data;
  const act = (name: string) => void action.run(async () => { await workforce.actTask(t, name, answer); task.reload(); });
  return <WorkPage title={t.title} sub={<><span className={`wf-status ${t.status}`}>{t.status.replace('_', ' ')}</span> · Updated {when(t.updated_at)}</>}>
    <Link className="wf-back" to={`/projects/${t.project_id}/tasks`}>← Project tasks</Link>
    {task.error && <Err error={task.error} />}{action.error && <Err error={action.error} />}
    <div className="wf-toolbar">{t.status === 'planned' && <button disabled={action.busy} onClick={() => act('start')}>Start task</button>}{['failed', 'cancelled'].includes(t.status) && <button disabled={action.busy} onClick={() => act('retry')}>Retry task</button>}{!['done', 'cancelled'].includes(t.status) && <button disabled={action.busy} onClick={() => act('cancel')}>Cancel task</button>}<button onClick={task.reload}>Reload latest revision</button></div>
    {['done', 'failed'].includes(t.status) && !t.acknowledged && <button disabled={action.busy} onClick={() => act('acknowledge')}>Mark reviewed in Attention</button>}
    <div className="wf-grid"><section className="wf-panel"><h2>Brief</h2><Markdown text={t.instructions} /><h3>Done means</h3><ul>{t.acceptance_criteria?.map((c, i) => <li key={i}>{c}</li>)}</ul><h3>Dependencies</h3>{t.depends_on?.length ? t.depends_on.map(id => <Link className="wf-dependency" key={id} to={`/tasks/${id}`}>{id} →</Link>) : <p>No prerequisites</p>}</section>
      <section className="wf-panel"><h2>Progress</h2><p>Assigned to {t.assignee_bot_id}</p>{t.checkpoint !== undefined && <><h3>Checkpoint</h3><p>Next browser step: {t.checkpoint + 1}</p></>}{t.error && <p className="wf-danger">{t.error}</p>}{t.question && <><h3>Question from the worker</h3><Markdown text={t.question} /></>}
        {t.status === 'blocked' && <form className="wf-form" onSubmit={e => { e.preventDefault(); act('answer'); }}><Field label="Answer / resume instruction"><textarea required value={answer} onChange={e => setAnswer(e.target.value)} /></Field><button disabled={action.busy}>Answer and resume</button><Link to={`/projects/${t.project_id}/computer`}>Open browser workspace →</Link></form>}
        {t.progress && <Markdown text={t.progress} />}
        {t.parent_id && <Link to={`/tasks/${t.parent_id}`}>Delegated by parent task →</Link>}
        {t.status === 'blocked' && t.manual_step && <button disabled={action.busy} onClick={() => act('complete_step')}>I completed this manual browser step</button>}
        {t.status === 'needs_approval' && <div className="wf-form"><p>{t.question || 'Review the task instructions and requested browser action before continuing.'}</p><button disabled={action.busy} onClick={() => act('approve')}>Approve this task action</button><button disabled={action.busy} onClick={() => act('cancel')}>Deny and cancel task</button></div>}
      </section></div>
    <section className="wf-panel"><h2>Deliverables</h2>{t.result ? <Markdown text={t.result} /> : <p>No result yet. Completed work appears here.</p>}{t.artifacts?.map(a => <div className="wf-artifact" key={a.id}><b>{a.name}</b><span>{a.kind}</span>{artifactURL(t, a) ? <a href={artifactURL(t, a)}>Open deliverable →</a> : <code>{a.reference}</code>}</div>)}</section>
  </WorkPage>;
}

export function Attention() {
  const items = useWork(workforce.attention);
  return <WorkPage title="Attention" sub="Decisions, blockers, and finished work that need your eyes."><div className="wf-toolbar"><Link to="/projects">Projects →</Link><button onClick={items.reload}>Refresh</button></div>{items.error && <Err error={items.error} />}{!items.data && items.loading && <Spinner />}{items.data?.length === 0 && <Empty title="You're all caught up" hint="Questions, approvals, failures, and deliverables from your projects will appear here." />}
    <div className="wf-attention">{items.data?.map(item => <article className="wf-panel" key={item.id}><small>{item.kind} · {when(item.created_at)}</small><h2>{item.title}</h2><Markdown text={item.detail} />{item.prompt_id && !item.task_id && <PromptApproval ask={{id:item.prompt_id,reason:item.detail}} onAnswered={items.reload} />}<div className="wf-toolbar">{item.task_id && <Link to={`/tasks/${item.task_id}`}>{item.kind==='approval'?'Review task approval':'Open task'} →</Link>}{item.project_id && <Link to={`/projects/${item.project_id}/${item.kind === 'takeover' ? 'computer' : 'channel'}`}>Open project →</Link>}</div></article>)}</div>
  </WorkPage>;
}
