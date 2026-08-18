---
sidebar_position: 9
---

# Operator credentials and the two CAs

There are **two certificate authorities** in a lobslaw cluster, and the
separation between them is load-bearing. This page explains what each one
signs, why they are not the same key, and how an operator gets a credential
without a private key ever crossing the network.

## The short version

| | Cluster CA | Operator CA |
|---|---|---|
| Signs | **Nodes** | **People** |
| Key lives | Offline, on the machine you ran `ca-init` on | **Online**, on the node |
| Certificate carries | `ServerAuth` + `ClientAuth`, DNS SANs | `ClientAuth` only, `OU=operator`, no SAN |
| Holder can | Join raft, replicate, serve peers | Administer; **refused on the raft transport** |
| If the key leaks | Attacker can manufacture peers — total compromise | Attacker can impersonate operators — bad, bounded |

A node holds an online signing key. That is a deliberate trade, and the whole
point of it being a *separate* key is that the thing it can mint is bounded.

## Why not one CA with an intermediate

That was the first design, and it does not work here.

`cluster ca-init` emits a CA with `MaxPathLen: 0`. In X.509 that means the CA
may sign end-entity certificates and **nothing else** — no subordinate CAs. Go's
verifier rejects a chain through it outright:

```
x509: too many intermediates for path length constraint
```

Raising the path length means reissuing the root, and reissuing the root means
reissuing **every node certificate** — a full cluster credential rotation before
enrolment would work at all.

So operator certificates chain to their own root instead. The security property
is the one an intermediate would have given, arrived at more directly:

- the online key mints `ClientAuth` `OU=operator` credentials
- it **cannot** mint a node certificate, because peers verify each other against
  the *cluster* CA and this key is not in that chain

## The pools are kept apart

A node's TLS configuration uses two different certificate pools, and mixing them
would undo everything above:

| Pool | Contains | Used for |
|---|---|---|
| `ClientCAs` | cluster CA **+** operator CA | verifying certificates presented *to* this node |
| `RootCAs` | cluster CA **only** | verifying certificates presented *by* whatever this node dials |

An operator certificate is `ClientAuth`-only, so it could not be presented by a
server in any case — but the pools are separate so that guarantee does not rest
on that alone. `TrustOperatorCA` never touches `RootCAs`, and builds a fresh
merged pool rather than appending onto the cluster one.

## Two ways to tell an operator certificate

`IsOperatorCert` reads the `OU=operator` string in the subject.
`ChainsToOperatorCA` reads **which key actually signed it**.

The second is the stronger check, because a subject is whatever the certificate
says about itself. A node certificate can be issued with `OU=operator` and will
fool the first; it cannot fool the second, because the cluster CA is not the
operator root.

The raft transport uses the OU check to refuse operators — enforced at the
**server**, on the streaming interceptor as well as the unary one, because
raft's transport is streaming and a unary-only guard would cover nothing that
matters. An unidentified caller is refused rather than admitted: mTLS is
mandatory on that listener, so a call arriving with no verified chain is not a
configuration this cluster has.

## Getting a credential onto a laptop

Two routes, and the default should be the second.

### `cluster export-operator` — bootstrap only

```bash
lobslaw cluster export-operator alice \
  --ca-cert certs/ca.pem --ca-key certs/ca-key.pem \
  --out ./alice
```

Generates a keypair **centrally** and writes both halves. It runs on the machine
holding the cluster CA private key, and the resulting `operator-key.pem` then
has to reach the laptop somehow.

That travelling private key is the weakness. Do not send it over a chat channel:
it would land in a message store, a phone backup, the provider's servers, and —
in this system specifically — the cluster's own transcripts, where `lobslaw
session show` will print it back.

Use this for the **first** operator, who has nobody to approve them. After that,
use enrolment.

*(`cluster sign-operator` still works and prints a deprecation note. It was
renamed because "sign" understated what it does.)*

### `lobslaw enrol` — the key never moves

```bash
# on the laptop, which has no credential yet
lobslaw enrol request \
  --addr node.example.com:9091 \
  --ca-cert ca.pem \
  --name alice \
  --wait 5m
```

The laptop generates ed25519 locally, writes `operator-key.pem` with mode `0600`,
and sends only a **certificate signing request**. A CSR and a certificate are
both public; neither is any use without the private half that stayed behind.

It prints a fingerprint:

```
Request submitted: 4f2a9c...
Private key:       ~/.config/lobslaw/operator-key.pem (never sent)

Fingerprint:       SHA256:a3:9f:...:b1
```

Then somebody approves it. Either from a chat:

> A laptop is asking for an operator credential.
>
> Name requested: alice
>
> Fingerprint:
> SHA256:a3:9f:...:b1
>
> Approve ONLY if that matches what they read you. An operator credential can
> administer this cluster.
>
> \[ Approve ] \[ Deny ]

…or from a machine that already holds a credential:

```bash
lobslaw enrol list
lobslaw enrol approve 4f2a9c... --fingerprint SHA256:a3:9f:...:b1
```

The chat question goes to whoever holds the **owner** scope, and **only they can
answer it** — the button is visible to everyone in a group chat, but a tap from
anyone else is refused and told why. There is no "always" button: a standing
authority to admit anyone who asks is not a thing this should be able to
express.

The prompt is raised *after* the request is durably queued and never fails the
submission. A channel outage means nobody was notified, not that the enrolment
was lost — it is still in `enrol list` and still approvable from the CLI.

**Read the fingerprint aloud.** It is the only thing distinguishing the request
you were told about from another that arrived at the same moment. `--fingerprint`
is required to approve and is checked at the server, so a request that changed
between reading it and approving it is refused rather than approved in place of
the one you verified.

Denial does not require a fingerprint — refusing something you cannot identify is
the safe direction, and demanding one to say *no* would leave junk requests
un-closable.

The approver may rename: `--name` issues under a different name from the one
requested. The requested and issued names are both recorded, so the difference is
visible afterwards.

## What enrolment still needs out of band

One thing: **the cluster CA certificate**, passed as `--ca-cert`.

Without it the laptop cannot tell your cluster from anything else answering on
that address, and would happily enrol against an impostor. The CA certificate is
public, so it can be emailed, pasted, or committed — it just has to arrive by
some path an attacker does not control.

## The enrolment listener

`Submit` and `Poll` are served on their own listener, configured with:

```toml
[cluster.mtls]
enrol_addr = ":9091"
```

It is the one surface that **cannot** require a client certificate, because the
caller is asking for the certificate it would present. Rather than relax the
cluster listener's "every caller presents a verified certificate" guarantee, this
gets its own listener serving exactly two RPCs. `List` and `Decide` are
unreachable there structurally, not by convention.

Leave `enrol_addr` empty to disable enrolment entirely. Existing operator
credentials keep working; only the submit path goes away.

## Lifetimes and expiry

- Operator certificates default to **90 days** — shorter than a node's, because a
  person's credential lives on a laptop that travels. Override per-issue with
  `--valid-for`, or cluster-wide with `enrol_valid_for`.
- Pending enrolment requests expire after **30 minutes**. An operator returning to
  a week-old request has no idea whether the laptop that asked is still the one
  they would be admitting.
- The expiry sweeper is leader-pinned. A timer would be per-process, so a request
  created on a node that then died would stay approvable forever.

## If something is compromised

| What leaked | Consequence | Response |
|---|---|---|
| An operator's private key | That person's access, until expiry | Rotate the operator CA; every operator re-enrols |
| The **operator CA** key (i.e. a node) | Attacker mints operator credentials — can administer, **cannot join raft** | Rotate the operator CA; cluster CA and node identities unaffected |
| The **cluster CA** key | Attacker mints node certificates — can join raft as a voter and replicate everything | Full rebuild. This is why the key stays offline. |

There is no per-certificate revocation list today. Rotation is the mechanism, and
the separation of the two CAs is what makes rotating the online one cheap.

## What is deliberately not possible

**A node cannot enrol another node.** The same CSR-and-approve shape would work,
and it would solve a real problem — UDP broadcast does not cross subnets, so a
remote node already needs `seed_nodes` plus a hand-copied certificate. But if the
online key could sign node certificates, a compromised node would mint a voting
peer, that peer would receive a full replica of memory, sessions and credentials,
and it could mint more peers. Unbounded from one compromise.

There is also a proportionality point. A chat-channel tap is a reasonable bar for
"let alice's laptop read the cluster". It is not a reasonable bar for "add a
voting replica that receives a copy of everything".

Cross-network node enrolment, if it lands, will queue a request that is signed at
the **offline** CA. Nodes are rare; operators are not.

## Discovery is a hint, not a trust boundary

Worth stating because the two are easy to conflate. UDP broadcast tells a node
where peers might be. It grants nothing. The actual join goes through `AddMember`
over mTLS, so a joining node must already hold a certificate signed by the
cluster CA.

Finding peers is automatic. Being trusted by them is not, and never has been.

## Reference

- `pkg/mtls/operator_ca.go` — the operator root, CSR signing, chain checks
- `pkg/mtls/mtls.go` — `TrustOperatorCA`, the pool separation, `EnrolmentServerConfig`
- `internal/memory/enrolments.go` — the request queue and its compare-and-set
- `internal/node/wire_enrolment.go` — the separate listener and its narrowed surface
- `cmd/lobslaw/enrol.go` — the laptop side
