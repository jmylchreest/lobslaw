# Working on a remote

`remote_ssh` runs a command on a machine that is not this one.
`remote_scp` moves one file between here and there. This is how real
development work gets done — and there are three rules that make the
difference between using them well and losing a day's work.

## 1. Nothing you have not pushed is real

The remote's disk is a **cache, not a record**. The pod can be
rescheduled, the workspace reclaimed, the session cleaned up — none of
these announce themselves, and all of them take uncommitted work with
them.

So: create a branch before you start, commit early, push often. Before
anything destructive — a rebase, a force-push, a dependency update that
rewrites a lockfile — run `git status` first and read it.

If you are about to say "I'll commit it at the end", commit it now
instead.

## 2. Edit code on the remote, not here

The tempting shortcut is to compose a file locally and `remote_scp` it
across, or to write it inline:

```
remote_ssh: cat > main.go <<'EOF' ...
```

**Don't.** Not for a one-line change, not when the fix is obvious, not
when it would be quicker.

A heredoc gets you no build, no test, no type-check, and no second look
at what you wrote. You will get the package name wrong, or an import,
or the surrounding function's signature, and you will not find out
until something else fails confusingly later. Editing on the machine
that has the compiler means the compiler is part of the loop.

Use `remote_scp` for artefacts — a log, a patch, a generated file you
are handing to the user. Not for source you are authoring.

## 3. One task, one directory

Work under `/workspace/tasks/<slug>`, one directory per task, `slug`
lowercase with dashes. Two tasks sharing a directory means two sets of
uncommitted changes on one branch, and the only way out is to read the
diff and guess.

Say what you are doing when you start — repo, branch, task slug — so
there is a record if the session is interrupted.

## Choosing a remote

`remote_ssh`'s tool description lists what is configured and what each
one is for. Match the stack to the work: the Go toolchain is on the Go
remote and not on the others. If nothing matches the work you have been
asked to do, say so rather than improvising on the closest one — a
missing toolchain is a configuration gap the operator needs to hear
about, not an obstacle to route around.

## What the guards will refuse, and why not to route around them

`remote_scp` applies the same path rules as `read_file` and
`write_file`, in whichever direction it is moving:

- **uploading** reads locally, so a cluster-internal path — a TLS key,
  the memory key, a Raft snapshot — is refused. That refusal is
  compiled in.
- **downloading** writes locally, so it will not overwrite one either;
  a remote choosing what this node reads back is the same problem
  wearing a different hat.

If you hit one of those, the answer is not another path to the same
file. Say what you were trying to do and stop.

Likewise, `remote_ssh` reports a **non-zero exit code as a result, not
an error**. A failing build is the answer to "does this build". Read
the stderr and fix the code; do not retry the same call hoping for a
different answer, and do not conclude the remote is broken.

## Sanity-checking a remote on first contact

A toolchain that answers `--version` is not a toolchain that works:
version output reads the binary, while builds read the environment. If
`go version` works but `go build` cannot find its module cache, the
environment is not being propagated to the SSH session.

Worth one command before real work:

```
uname -a; ls /workspace; git --version
echo "GOMODCACHE=${GOMODCACHE:-<unset>} CARGO_HOME=${CARGO_HOME:-<unset>}"
```

An `<unset>` where you expected a path is the thing to report — it will
otherwise surface later as a build failure that looks like a code
problem.
