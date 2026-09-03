#!/usr/bin/env python3
"""Poll several models on what package-manager commands DO.

Not a benchmark — a research aid for authoring the static table. The
models supply factual recall about commands nobody has catalogued yet;
consensus is a candidate, disagreement is a flag for review, and the
final call stays with a human reading the result.
"""
import json, os, statistics, sys, time, urllib.request, collections
from concurrent.futures import ThreadPoolExecutor

QWEN_KEY = os.environ["LOBSLAW_QWEN_API_KEY"]
MINIMAX_KEY = os.environ["LOBSLAW_MINIMAX_API_KEY"]
QWEN_URL = "https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1/chat/completions"
MINIMAX_URL = "https://api.minimax.io/v1/chat/completions"

# The models that were clean on safety in the plan benchmark, spread
# across three vendors so a shared blind spot is less likely.
MODELS = {
    "qwen3.8-flash": (QWEN_URL, QWEN_KEY),
    "qwen3.7-max": (QWEN_URL, QWEN_KEY),
    "deepseek-v4-flash-0731": (QWEN_URL, QWEN_KEY),
    "glm-5.2": (QWEN_URL, QWEN_KEY),
    "MiniMax-M3": (MINIMAX_URL, MINIMAX_KEY),
}

SYSTEM = """You catalogue what a shell command DOES. Reply with JSON only, no prose:

{"labels": ["..."]}

Choose every label that applies, from exactly this set:

reads      — inspects state and changes nothing
writes     — creates, copies, appends to or edits files, recoverably
deletes    — removes data. Undone by a backup, or not at all
disrupts   — takes something down: restarts/stops a service, kills a process,
             unmounts a filesystem, flushes a firewall. Undone in seconds
network    — contacts another host: fetching, uploading, running something remotely
privilege  — needs root, or changes who may become root
unreadable — it runs code you cannot see in advance (an arbitrary build script,
             a fetched installer, a post-install hook running as root)

Be complete and be literal. If a command installs a system package it almost
certainly needs root AND fetches from a remote AND writes to the filesystem —
say all three. If it compiles something from a source recipe downloaded at run
time, that recipe is code you cannot see, so include unreadable."""

COMMANDS = [
    # pacman, and the AUR helpers that wrap it
    "pacman -S firefox", "pacman -Syu", "pacman -Sy", "pacman -R firefox",
    "pacman -Rns firefox", "pacman -Q", "pacman -Qi firefox", "pacman -Ss firefox",
    "pacman -Sc", "pacman -Scc", "pacman -U ./local-pkg.tar.zst",
    "paru -S firefox", "paru -Syu", "paru -Ss firefox", "paru -Q",
    "yay -S firefox", "yay -Syu", "yay -Ss firefox", "yay -Q",
    # rpm / dpkg, which are flag-driven
    "rpm -i pkg.rpm", "rpm -e pkg", "rpm -q pkg", "rpm -qa", "rpm -U pkg.rpm", "rpm -V pkg",
    "dpkg -i pkg.deb", "dpkg -r pkg", "dpkg --purge pkg", "dpkg -l", "dpkg -L pkg", "dpkg -S /bin/ls",
    # the subcommand-driven family
    "apt-get install -y curl", "apt-get remove curl", "apt-get update", "apt-get autoremove",
    "dnf install curl", "dnf remove curl", "zypper install curl", "apk add curl", "apk del curl",
    # others worth covering
    "emerge www-client/firefox", "emerge --depclean",
    "xbps-install -S firefox", "xbps-remove firefox",
    "nix-env -iA nixpkgs.firefox", "nix profile install nixpkgs#firefox",
    "brew install wget", "brew uninstall wget", "brew update",
    "snap install code", "snap remove code",
    "flatpak install flathub org.gimp.GIMP", "flatpak uninstall org.gimp.GIMP",
    "cargo install ripgrep", "gem install rails", "go install example.com/x@latest",
    "npm install -g typescript", "pipx install black",
    # the two known defects, re-checked
    "env", "apt-get install -y curl",
]

LABELS = {"reads", "writes", "deletes", "disrupts", "network", "privilege", "unreadable"}


def ask(model, command):
    url, key = MODELS[model]
    body = json.dumps({
        "model": model, "temperature": 0, "max_tokens": 512,
        "messages": [{"role": "system", "content": SYSTEM},
                     {"role": "user", "content": command}],
    }).encode()
    req = urllib.request.Request(url, data=body, headers={
        "Content-Type": "application/json", "Authorization": f"Bearer {key}"})
    try:
        with urllib.request.urlopen(req, timeout=90) as r:
            d = json.loads(r.read())
        content = (d["choices"][0]["message"] or {}).get("content") or ""
        i = content.find("{")
        j = content.rfind("}")
        got = json.loads(content[i:j + 1])
        return {l for l in got.get("labels", []) if l in LABELS}
    except Exception:
        return None


def poll(command):
    out = {}
    for m in MODELS:
        out[m] = ask(m, command)
    return command, out


if __name__ == "__main__":
    results = {}
    with ThreadPoolExecutor(max_workers=6) as ex:
        for cmd, per in ex.map(poll, COMMANDS):
            results[cmd] = {m: sorted(s) if s is not None else None for m, s in per.items()}
            print(".", end="", file=sys.stderr, flush=True)
    json.dump(results, open("pkg-poll.json", "w"), indent=1)
    print("\nwritten", file=sys.stderr)
