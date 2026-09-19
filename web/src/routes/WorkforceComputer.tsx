import { useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import { ApiError } from '../api';
import { Err, Spinner } from '../components/ui';
import { workforce, type Project } from '../workforce';
import { Field, useAction, useWork } from './Workforce';

const frameRefreshMillis = 4000;
const browserViewport = { width:1280,height:800 };
export function Computer({ project }: { project: Project }) {
  const state = useWork(() => workforce.computer(project.id), [project.id]), action = useAction();
  const [frame, setFrame] = useState(''), [frameError, setFrameError] = useState<Error | null>(null), [frameVersion, setFrameVersion] = useState(0);
  const [url, setURL] = useState(''), [selector, setSelector] = useState(''), [value, setValue] = useState(''), [key, setKey] = useState('Enter'), [name, setName] = useState(''), [saved, setSaved] = useState(false);
  const human = state.data?.control === 'human';
  useEffect(() => {
    if (!state.data?.available) { setFrame(''); return; }
    const controller = new AbortController(); let current = '', stopped = false;
    async function capture() {
      try {
        const res = await fetch(`/v1/computers/${encodeURIComponent(project.id)}/screenshot`, { credentials: 'include', signal: controller.signal, cache: 'no-store' });
        if (!res.ok) throw new ApiError(res.status, await res.text());
        const blob = await res.blob(); if (stopped) return;
        const next = URL.createObjectURL(blob); if (current) URL.revokeObjectURL(current); current = next;
        setFrame(next); setFrameError(null);
      } catch (e) { if (!stopped) setFrameError(e as Error); }
      finally { if (!stopped) timer = setTimeout(capture, frameRefreshMillis); }
    }
    let timer: ReturnType<typeof setTimeout>;
    void capture();
    return () => { stopped = true; clearTimeout(timer); controller.abort(); if (current) URL.revokeObjectURL(current); };
  }, [project.id, state.data?.available, frameVersion]);
  const operate = (command: { action: string; selector?: string; value?: string; url?: string; x?:number; y?:number }) => void action.run(async () => {
    await workforce.computerAction(project.id, command); setValue(''); state.reload(); setFrameVersion(v => v + 1);
  });
  return <div className="wf-computer">
    <div className="wf-toolbar"><h2>Browser workspace</h2><span className={`wf-status ${human ? 'blocked' : 'ready'}`}>{human ? 'You have control · bot paused' : 'Bot control'}</span></div>
    <p className="wf-muted">A private, persistent browser for this project. Frames and profile data stay on the browser backend. This is a browser, not a desktop.</p>
    {state.error && <Err error={state.error} />}{action.error && <Err error={action.error} />}
    {!state.data && state.loading && <Spinner />}
    <div className="wf-toolbar">
      <button disabled={action.busy || !state.data} onClick={() => operate({ action: state.data?.available ? 'stop' : 'start' })}>{state.data?.available ? 'Close browser' : 'Open browser'}</button>
      <button disabled={action.busy || !state.data} onClick={() => operate({ action: human ? 'release' : 'takeover' })}>{human ? 'Return control to bot' : 'Take control'}</button>
      <button disabled={!frame || action.busy} onClick={() => setFrameVersion(v => v + 1)}>Refresh frame</button>
    </div>
    {frameError && <Err error={frameError} />}
    <div className="wf-screen">{frame ? <img src={frame} alt="Current project browser screenshot" onClick={event=>{if(!human||action.busy)return;const rect=event.currentTarget.getBoundingClientRect();operate({action:'click',x:(event.clientX-rect.left)*browserViewport.width/rect.width,y:(event.clientY-rect.top)*browserViewport.height/rect.height});}} style={{cursor:human?'crosshair':'default'}} /> : <div className="wf-screen-empty"><b>{state.data?.available ? 'Waiting for the browser frame…' : 'Browser is closed'}</b><p>Open a configured workspace to see its real browser. Missing Chromium, sandbox, or egress configuration is reported as unavailable.</p></div>}</div>
    {human && <p className="wf-muted">Click or tap the screenshot to focus an element. Enter private text below, or use a selector for keyboard-accessible precision.</p>}
    {human && <div className="wf-grid">
      <form className="wf-panel wf-form" onSubmit={e => { e.preventDefault(); operate({ action: 'navigate', url }); }}><h3>Navigate</h3><Field label="Website URL"><input type="url" required placeholder="https://example.com" value={url} onChange={e => setURL(e.target.value)} /></Field><button disabled={action.busy}>Go to website</button></form>
      <form className="wf-panel wf-form" onSubmit={e => { e.preventDefault(); operate({ action: 'click', selector }); }}><h3>Interact with the page</h3><Field label="Element selector"><input placeholder="Leave empty to type into the focused element" value={selector} onChange={e => setSelector(e.target.value)} /></Field><Field label="Private text entry (never recorded)"><input type="password" autoComplete="off" value={value} onChange={e => setValue(e.target.value)} /></Field><div className="wf-toolbar"><button disabled={action.busy || !selector}>Click element</button><button type="button" disabled={action.busy} onClick={() => operate({ action: 'fill', selector, value })}>Enter text</button></div><Field label="Key"><select value={key} onChange={e => setKey(e.target.value)}>{['Enter', 'Tab', 'Escape', 'Backspace', 'ArrowUp', 'ArrowDown', 'ArrowLeft', 'ArrowRight', 'Space'].map(k => <option key={k}>{k}</option>)}</select></Field><button type="button" disabled={action.busy} onClick={() => operate({ action: 'press', value: key })}>Press key</button></form>
    </div>}
    <section className="wf-panel wf-form"><h2>Teach a routine</h2><p>Take control and record a demonstration. Every text entry becomes a manual checkpoint; passwords and secret values are never saved as routine input. URL query strings become manual checkpoints too.</p>
      <div className="wf-toolbar"><button disabled={!human || action.busy || state.data?.recording} onClick={() => { setSaved(false); operate({ action: 'record' }); }}>{state.data?.recording ? 'Recording your actions…' : 'Start new recording'}</button><button disabled={action.busy || !state.data?.steps?.length} onClick={() => operate({ action: 'discard' })}>Discard recording</button></div>
      <ol className="wf-steps">{state.data?.steps?.map((step, i) => <li key={i}><b>{step.action}</b> {step.sensitive ? 'Manual entry required — no value saved' : step.url || step.selector || step.value}</li>)}</ol>
      <form className="wf-form" onSubmit={e => { e.preventDefault(); void action.run(async () => {
        await workforce.draft(project.id, { name, description: 'Demonstrated in the project browser', instructions: 'Follow the reviewed browser steps. Pause for manual checkpoints.', steps: state.data?.steps ?? [] }); setSaved(true);
      }); }}><Field label="Routine name"><input required value={name} onChange={e => setName(e.target.value)} /></Field><button disabled={action.busy || !state.data?.steps?.length || saved}>Save routine draft</button></form>
      {saved && <p role="status">Draft saved. <Link to={`/projects/${project.id}/routines`}>Review and explicitly approve it in Routines →</Link></p>}
    </section>
  </div>;
}
