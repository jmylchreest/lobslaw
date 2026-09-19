import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { Err, Empty, when } from '../components/ui';
import { workforce, type Project, type Workflow } from '../workforce';
import { Field, useAction, useWork } from './Workforce';

export function Workflows({ project }: { project: Project }) {
  const routines = useWork(() => workforce.routines(project.id), [project.id]), triggers = useWork(() => workforce.triggers(project.id), [project.id]), action = useAction(), navigate = useNavigate();
  const [name, setName] = useState(''), [instructions, setInstructions] = useState(''), [triggerName, setTriggerName] = useState(''), [routineID, setRoutineID] = useState('');
  const [events, setEvents] = useState<Record<string, string>>({}), [receipt, setReceipt] = useState('');
  return <>
    <div className="wf-toolbar"><h2>Routines & triggers</h2><button onClick={() => { routines.reload(); triggers.reload(); }}>Refresh</button></div>
    <p className="wf-muted">Review once, delegate repeatedly. Editing an approved definition invalidates its approval.</p>
    {routines.error && <Err error={routines.error} />}{action.error && <Err error={action.error} />}
    {routines.data?.length === 0 && <Empty title="No routines yet" hint="Write a task routine below, or demonstrate browser work in Computer." />}
    {routines.data?.map(routine => <RoutineCard key={routine.id} routine={routine} changed={routines.reload} />)}
    <details className="wf-panel"><summary>Create a task routine draft</summary><form className="wf-form" onSubmit={e => { e.preventDefault(); void action.run(async () => {
      await workforce.draft(project.id, { name, instructions, description: '', steps: [] }); setName(''); setInstructions(''); routines.reload();
    }); }}><Field label="Routine name"><input required value={name} onChange={e => setName(e.target.value)} /></Field><Field label="Instructions"><textarea required value={instructions} onChange={e => setInstructions(e.target.value)} /></Field><button disabled={action.busy}>Save draft for review</button></form></details>
    <h2>Event triggers</h2><p className="wf-muted">A repeated event ID creates one task. Reuse the ID when retrying the same event.</p>
    {triggers.error && <Err error={triggers.error} />}
    <form className="wf-panel wf-form" onSubmit={e => { e.preventDefault(); void action.run(async () => {
      await workforce.createTrigger(project.id, { name: triggerName, routine_id: routineID, enabled: true }); setTriggerName(''); triggers.reload();
    }); }}><Field label="Trigger name"><input required value={triggerName} onChange={e => setTriggerName(e.target.value)} /></Field><Field label="Approved routine"><select required value={routineID} onChange={e => setRoutineID(e.target.value)}><option value="">Select a routine</option>{routines.data?.filter(r => r.status === 'approved').map(r => <option value={r.id} key={r.id}>{r.name}</option>)}</select></Field><button disabled={action.busy || !routineID}>Create trigger</button></form>
    {triggers.data?.map(trigger => <form key={trigger.id} className="wf-panel wf-form" onSubmit={e => { e.preventDefault(); void action.run(async () => {
      const res = await workforce.fireTrigger(trigger.id, events[trigger.id] || ''); setReceipt(res.duplicate ? 'Already received — opening the existing task.' : 'Event accepted — task created.'); navigate(`/tasks/${res.task.id}`);
    }); }}><h3>{trigger.name}</h3><p>{trigger.enabled ? 'Enabled' : 'Disabled'} · Last fired {when(trigger.last_fired_at) || 'never'}</p><code>{`POST /v1/triggers/${trigger.id}/fire`}</code><Field label={`Event ID for ${trigger.name}`}><input required value={events[trigger.id] || ''} onChange={e => setEvents({ ...events, [trigger.id]: e.target.value })} placeholder="invoice-2026-001" /></Field><button disabled={action.busy || !trigger.enabled}>Deliver event</button></form>)}
    {receipt && <p role="status">{receipt}</p>}
  </>;
}

function RoutineCard({ routine, changed }: { routine: Workflow; changed: () => void }) {
  const action = useAction(), navigate = useNavigate(), [editing, setEditing] = useState(false), [instructions, setInstructions] = useState(routine.instructions), [steps, setSteps] = useState(JSON.stringify(routine.steps ?? [], null, 2)), [reviewed, setReviewed] = useState(false), [schedule,setSchedule] = useState(routine.schedule ?? '');
  const [editRevision,setEditRevision]=useState(routine.revision);
  useEffect(()=>setReviewed(false),[routine.revision]);
  const toggleEdit=()=>{if(!editing){setInstructions(routine.instructions);setSteps(JSON.stringify(routine.steps??[],null,2));setSchedule(routine.schedule??'');setEditRevision(routine.revision);}setEditing(!editing);};
  return <article className="wf-panel wf-form"><div className="wf-toolbar"><h3>{routine.name}</h3><span className={`wf-status ${routine.status === 'approved' ? 'done' : 'planned'}`}>{routine.status}</span><small>Revision {routine.revision}</small></div><p>{routine.description}</p>
    {editing ? <form className="wf-form" onSubmit={e => { e.preventDefault(); void action.run(async () => {
      const parsed: unknown = JSON.parse(steps); if (!Array.isArray(parsed)) throw new Error('Steps must be an array.');
      await workforce.editRoutine({...routine,revision:editRevision}, 'edit', { instructions, steps: parsed, schedule });setEditing(false); changed();
    }); }}><Field label="Routine instructions"><textarea required value={instructions} onChange={e => setInstructions(e.target.value)} /></Field><Field label="Browser steps (JSON; no credentials)"><textarea rows={8} value={steps} onChange={e => setSteps(e.target.value)} /></Field><Field label="Schedule (cron, optional)"><input value={schedule} onChange={e=>setSchedule(e.target.value)} placeholder="0 9 * * 1-5" /></Field><p>Saving changes returns this routine to draft and invalidates its old approval.</p><button disabled={action.busy}>Save changes as draft</button></form> : <><p>{routine.instructions}</p><p>Schedule: {routine.schedule || 'Run on demand'}</p><ol className="wf-steps">{routine.steps?.map((step, i) => <li key={i}><b>{step.action}</b> {step.sensitive ? 'Manual checkpoint' : step.url || step.selector || step.value}<small>{step.description}</small></li>)}</ol></>}
    {action.error && <Err error={action.error} />}
    {routine.status !== 'approved' && !editing && <label className="wf-check"><input type="checkbox" checked={reviewed} onChange={e => setReviewed(e.target.checked)} />I reviewed this exact definition and its browser actions.</label>}
    <div className="wf-toolbar"><button disabled={action.busy} onClick={toggleEdit}>{editing ? 'Cancel edit' : 'Edit definition'}</button>{routine.status === 'approved' ? <><button disabled={action.busy || editing} onClick={() => void action.run(async () => { const task = await workforce.runRoutine(routine.id); navigate(`/tasks/${task.id}`); })}>Run as task</button><button disabled={action.busy} onClick={() => void action.run(async () => { await workforce.editRoutine(routine, 'disable'); changed(); })}>Disable</button></> : <button disabled={!reviewed || action.busy || editing} onClick={() => void action.run(async () => { await workforce.editRoutine(routine, 'approve'); changed(); })}>Approve definition</button>}</div>
  </article>;
}
