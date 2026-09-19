# Project browser workspaces

The **Computer** tab is a real, headless Chromium browser belonging to you and
one project. It shows screenshots, accepts clicks and keyboard/text actions, and
keeps that project's login session between browser restarts. It is a browser
workspace, not a remote desktop or VM.

## Set up the browser backend

Configure the compute backend that runs your workforce tasks. A web-only node
forwards Computer requests through its existing `[ui-web].backend` connection.
Enable `compute-teams` and `ui-web` explicitly; `--all` does not enable them.

Requirements:

- Linux with working unprivileged user, mount, PID and network namespaces.
- Landlock available to the subprocess. A missing filesystem sandbox is an error.
- Node.js, Playwright, Chromium and `iproute2`, plus Chromium's shared libraries
  and fonts. The ordinary lobslaw binary does not bundle these executables.
- A dedicated, mode-0700 local directory owned by the lobslaw service account.
- The existing egress proxy's Unix socket and an explicit browser hostname list.

The validated runtime is Node **24.13.1**, Playwright
**1.59.0-alpha-1771104257000**, and Playwright Chromium build **1228**. Pin the
runtime you deploy and keep its package lockfile. For example, on a Debian/Ubuntu
host with Node already installed, provision a dedicated runtime prefix:

```sh
sudo install -d -m 0755 /opt/lobslaw-browser
sudo npm install --prefix /opt/lobslaw-browser --save-exact \
  playwright@1.59.0-alpha-1771104257000
sudo env PLAYWRIGHT_BROWSERS_PATH=/opt/lobslaw-browser/browsers \
  node /opt/lobslaw-browser/node_modules/playwright/cli.js install --with-deps chromium
sudo apt-get install iproute2
sudo install -d -o lobslaw -g lobslaw -m 0700 /var/lib/lobslaw-computer
```

Use the actual service account in the last command. Check your installed Node,
Chromium and `ip` paths; they are explicit configuration, not PATH discovery:

```toml
[compute-teams]
enabled = true

[ui-web]
enabled = true

[security]
egress_uds_path = "/run/lobslaw/egress.sock"

[computer]
enabled = true
root = "/var/lib/lobslaw-computer"
node = "/usr/bin/node"
playwright = "/opt/lobslaw-browser/node_modules/playwright"
chromium = "/opt/lobslaw-browser/browsers/chromium-1228/chrome-linux64/chrome"
ip = "/usr/sbin/ip"
allow_hosts = ["example.com", "www.example.com"]
read_paths = [
  "/usr", "/lib", "/lib64",
  "/etc/fonts", "/etc/ssl", "/etc/ld.so.cache",
  "/proc", "/sys", "/opt/lobslaw-browser"
]
```

Merge these sections with your existing config. Existing authenticated web
channel setup, mTLS and user enrolment still apply. If Node lives outside `/usr`,
add its runtime directory to `read_paths`. Do not include the browser profile root,
your home directory, the lobslaw data directory, or a parent containing those
directories. `/proc` is remounted inside the browser's private PID namespace; it
does not expose the host's process environment. The service adds write access only
to the selected project profile, a private temporary directory and required devices.

`allow_hosts` generates the `computer` egress role. Include the actual site, login
provider and asset hostnames your workflow needs. Private-address sites also need
the existing explicit `security.egress_allow_ranges` setting; listing a hostname
does not silently open a private network. Requests, redirects and subresources all
use the same proxy. A network namespace has no external interface, so disabling
browser proxy settings cannot create a direct connection.

For containers, install the same runtime in your image and give the service a
private persistent volume for `computer.root`. The container's kernel/security
profile must permit the required namespace creation, private procfs mount and
Landlock calls. A runtime that blocks them produces an unavailable error; the
browser does not retry without containment. Limit container memory/CPU at the
deployment level. This feature does not create cgroups itself.

Use one configured browser backend for a project's workforce workers and web
channel. Profiles are local to that backend; they are not automatically replicated
or migrated between compute nodes. Only one service process may own a profile root.

## Work with the team

1. Open **Projects**, create a project, and select its coordinator and teammates.
2. Discuss its brief in **Channel**. One selected bot answers each turn.
3. Use **Tasks** to delegate work, set acceptance criteria and choose dependencies.
   Open a task to start it and follow its actual status, checkpoint and result.
4. Use **Attention** for pending task approvals, questions, failures, completed
   deliverables and browsers still under your control.

All records are authorized against the project's human owner. A teammate's tools
allowlist and normal `tool:exec` policy still apply to automated browser actions
(`browser_navigate`, `browser_click`, `browser_fill`, `browser_press`,
`browser_wait`, `browser_capture`). Routine approval does not grant tool access.
An action requiring confirmation remains a pending task approval until you review
and approve that exact task action.

## Take control and sign in

1. Open the project's **Computer** tab and choose **Open browser**.
2. Choose **Take control**. The acknowledgement means any in-flight bot action
   has finished and subsequent bot actions are blocked.
3. Enter a website URL. Click/tap its screenshot to focus an element, then use
   **Private text entry** and **Enter text**. You can also specify a CSS/Playwright
   selector and use **Click element**. Key controls include Enter, Tab and arrows.
4. Sign in using the real website. Password input is masked in the console and
   text entry values are never included in recordings.
5. Choose **Return control to bot**. For a task waiting on a manual browser step,
   open that task and choose **I completed this manual browser step** to advance
   its checkpoint, or answer its question to retry the current step.

Control does **not** expire automatically. It survives service restart until you
return it, so a bot cannot resume against a half-completed login. **Close browser**
stops Chromium and frees an active browser slot while retaining its private profile.
Up to four browser workspaces may be active at a time.

Frames refresh every four seconds and after your actions. Screenshots are
authenticated, `no-store` responses; no public CDP or viewer port is exposed.
The console does not record video or offer file upload/download management.

## Teach and approve a routine

1. Take control in **Computer** and choose **Start new recording**.
2. Demonstrate navigation, clicks and key presses. Clicks are recorded as structural
   selectors, without field values or page text.
3. Every text entry becomes a **manual checkpoint**, including ordinary text
   fields. URLs containing query strings or fragments also become manual
   checkpoints. This avoids guessing which values are credentials.
4. Name the routine and choose **Save routine draft**. Review it in **Routines**.
5. Review the exact instructions and steps, check the review acknowledgement,
   then choose **Approve definition**. Only approved routines can run as tasks.
6. **Edit definition** can change instructions, browser steps and an optional cron
   schedule. Saving an edit invalidates approval and returns it to draft.
7. Use **Run as task**, or create a trigger referencing the approved routine. An
   event delivery uses an `event_id`; retrying that same ID opens the existing task.

Review structural selectors against the target site's stability. Site layout
changes can make a demonstrated click target different content; keep the routine
definition narrow, and require task-action approval for consequential operations.
Enter secrets only through private browser control, never in routine instructions,
JSON steps, project chat or task answers.

## Storage and troubleshooting

Browser profiles, cookies, local storage and local restore metadata stay beneath
`computer.root`. They are not stored in Raft, portable archives, task results or
LLM transcripts. Keep this directory out of agent-visible mounts and general
backup jobs that should not contain login sessions. Deleting a profile requires
signing in again; closing/reopening a browser preserves it.

| Symptom | Action |
|---|---|
| Computer returns 503 | Check executable paths, private root permissions, libraries, namespace/Landlock support, and the egress Unix socket. |
| Site navigation fails | Check its hostname/redirect/asset hosts against `allow_hosts`, then check any explicit private-network range. |
| Bot is blocked by human control | Finish your interaction, return control, then resume the task. |
| Another controller owns this root | Stop the other service or assign a separate local root; do not share browser profiles concurrently. |
| Workspace capacity reached | Close an inactive browser before opening another project. |
| Task or routine returns 409 | Reload the record, review its current revision, then retry the intended action. |
| Selector no longer works | Take control, inspect the current page and re-record/edit the routine. Re-approve the changed definition. |

The browser action timeout is 30 seconds and recordings are limited to 100 steps.
Failures never return placeholder screenshots or fabricated browser success.
