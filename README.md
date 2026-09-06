# veris

One CLI for the [Veris](https://veris.ai) dependency sandbox. It logs in to a
control plane, defines the environments a project tests against, starts
sandboxes of them, and routes the code under test at a sandbox from outside
the process: the code keeps its production hostnames, credentials and client
libraries, and every run ends with a receipt of what the sandbox received. A
single static binary with no runtime dependencies, so it drops into any
container image including Alpine and distroless.

## Why a proxy rather than a base-URL override

Overriding `stripe.api_base` or an equivalent means the code path under test is
not the code path that ships. For a product whose premise is catching
integration bugs that unit mocks miss, testing a modified code path
reintroduces the exact gap it exists to close. The proxy is what makes the test
faithful.

## Install

macOS and Linux:

```sh
curl -LsSf https://raw.githubusercontent.com/veris-ai/veris-cli/main/scripts/install.sh | sh
```

Windows:

```powershell
powershell -c "irm https://raw.githubusercontent.com/veris-ai/veris-cli/main/scripts/install.ps1 | iex"
```

The installer downloads the released static binary for this OS/arch into
`~/.local/bin` (`%LOCALAPPDATA%\Programs\veris-proxy` on Windows, kept under
its old name because it is already on the PATH of every machine that ran an
earlier installer). No root and no package manager, so it works the same on a
laptop, a CI runner, and inside a container build. The binary used to be
called `veris-proxy`; the installer leaves a `veris-proxy` shim beside `veris`
so scripts that still say so keep working.

| Variable | Effect |
|---|---|
| `VERIS_INSTALL_DIR` | Install somewhere else |
| `VERIS_PROXY_VERSION` | Pin a version, e.g. `v0.8.0` (default: latest) |

Binaries for every supported platform are also attached to each
[release](https://github.com/veris-ai/veris-cli/releases).

## The first ten minutes

Five commands, no ids typed:

```sh
veris login          # pair once: a code on screen, approved in the console
veris env create     # name the services; writes .veris/twin.yaml
veris up             # a sandbox of that environment, waited on until routable
veris run -- <cmd>   # the test command through the proxy, with a receipt
veris down           # delete the sandbox
```

`veris login` prints a pairing code and opens the console, where a signed-in
person approves it for one of their organisations. The key arrives on the next
poll and is saved to `~/.veris/twin.yaml` (mode 0600) under a profile:

```
$ veris login
Pairing this machine with Veris
  Profile  default
  API      https://svc.api.veris.ai
  Client   veris on victor-mbp

  Open   https://studio.veris.ai/connect?code=8FZS-YQKQ
  Code   8FZS-YQKQ
  (opened in your browser — pass --no-browser to skip)

✓ Approved for Acme
✓ Logged in as key 'veris on victor-mbp' (key_b8bx3hw6bx929g7eg1hmo, vsk_mi4pa0uo…)
✓ API key saved to /Users/victor/.veris/twin.yaml (profile 'default', mode 0600)
→ https://studio.veris.ai/overview
→ Next: veris env create
```

`veris env create` asks two questions on a TTY — the name, and a searchable
service picker — and takes both as flags off one. They are what an
environment IS, and they go to the server; the rest of what a start-up needs
is a flag, recorded in `.veris/twin.yaml` in this folder only when given.
Nothing else is asked, because `up` and `run` already have an answer for it:
without `--boot` a sandbox boots the bundle, without `--data` it seeds
nothing, and without `--command` a run takes its command after `--`. The
first environment of a project is its default.

```
$ veris env create checkout-svc --services stripe,postgres
✓ Environment created: 4h8k2m6n0p3r7t1v5x9z2b4d6 (checkout-svc: stripe, postgres)
✓ Added 'checkout-svc' to .veris/twin.yaml as the default
✓ Added .veris/twin.local.yaml to .gitignore (per-machine; holds sandbox ids)
→ https://studio.veris.ai/environments/4h8k2m6n0p3r7t1v5x9z2b4d6
→ Next: veris up
```

`veris up` deploys a sandbox of that environment, remembers its id for this
folder at once, waits until the control plane reports it ready *and* every
twin answers through the public gateway, then prints the env-var hint each
service carries:

```
$ veris up
Starting 'checkout-svc' (checkout-svc: stripe, postgres) · boot bundle · ttl 60 min
✓ Sandbox created: 7hqz4m2n9c1v5x8b3k6t0r2p4
  ✓ postgres  ready  (data plane; handed to the app, not proxied)
  ✓ stripe    routable  218 ms
  stripe     STRIPE_API_BASE=https://svc.api.veris.ai/s/7hqz4m2n9c1v5x8b3k6t0r2p4/stripe
  postgres   DATABASE_URL=postgresql://app:app@34.55.134.28:5432/sb7hqz4m2n…?sslmode=require
             (data plane; handed to the app, not proxied)
✓ Up: 7hqz4m2n9c1v5x8b3k6t0r2p4 is this folder's sandbox (expires 16:04 EDT)
→ https://studio.veris.ai/sandboxes/7hqz4m2n9c1v5x8b3k6t0r2p4
→ Next: veris run
```

The hints are for reading, not for pasting into the app. `veris run` routes
the command at this folder's sandbox and reports what the sandbox received.
The command comes after `--`, or from the project file when `env create
--command` recorded one, which is what makes a bare `veris run` work:

```
$ veris run -- pytest -q
veris: using sandbox 7hqz4m2n9c1v5x8b3k6t0r2p4 (this folder)
........                                                                 [100%]
8 passed in 4.21s
veris: the sandbox received 7 request(s):
  stripe                       7
```

`veris down` deletes the sandbox and clears the pointer. The TTL is the
backstop if it never runs.

Every command takes `--json` (the machine-readable body on stdout, progress on
stderr), `-q`, `--yes` for its confirmations, `--profile` and `--api-base`,
before or after the command word — except `run`, `serve` and `check`, which
take their own flags after it and refuse a global placed before it by name.
Any unambiguous prefix of a command works (`veris sand get`, `veris env c`).
Human output goes to stderr; stdout belongs to your command and to `--json`.

## Profiles and CI

A profile is one login: a control plane, a key, and the organisation the key
is bound to. `~/.veris/twin.yaml` holds them:

```yaml
active_profile: default
profiles:
  default:
    api_base: https://svc.api.veris.ai
    api_key: vsk_mi4pa0uo…             # 0600; printed only as a prefix
    console_url: https://studio.veris.ai
  dev:
    api_base: https://svc.dev.api.veris.ai
    console_url: https://studio.dev.veris.ai
    api_key: vsk_9a7b3c1d…
```

`veris login --profile dev --api-base https://svc.dev.api.veris.ai` pairs
against a second plane; the device-code response carries the matching console
URL, so pointing at dev pairs through `studio.dev.veris.ai` without a second
flag. `profile list`, `profile get`, `profile use NAME` and `profile delete
NAME` manage them. `whoami` names which credential a command would send and
where it came from; it is the first thing a broken CI job should run.
`logout` revokes the key on the plane and removes it locally (`--keep-key`
skips the revoke, for a key shared with CI).

Which profile a command uses: `--profile` → `VERIS_PROFILE` → the
environment's own `profile:` line in the project file → `active_profile` →
`default`.

CI never logs in. `VERIS_API_KEY` (plus `VERIS_API_BASE` for a plane other
than production) beats the profile on every command, and when it is set the
profile's key is never read. Where a file is wanted anyway:

```sh
printf '%s' "$VERIS_API_KEY" | veris login --key-stdin --profile ci
```

The key is verified against `/v1/me` before it is saved. A key given as a
positional `KEY` works too, with a warning that it lands in shell history.

## Environments and the project file

On the server an environment is only a named service set. Everything that
makes a start-up specific — TTL, boot source, data files, callback URL, proxy
defaults, test command — lives in a named config in `.veris/twin.yaml`, which
is committed and found by walking up from the current directory, so a
monorepo can carry one per service folder. Every line below `id:` is
optional, written only by the flag that names it:

```yaml
version: 1
project: checkout-svc
default: dev                        # used when nothing else names one
environments:
  dev:
    id: k3j2v0d8p1q7x9r2m5n8b4c6a   # the server environment: stripe, postgres
    ttl_minutes: 60                 # optional; without it the control plane's default
    boot: bundle                    # bundle | baseline | snapshot
    data:                           # rows of your own, added by up after boot
      - data/dev-customers.json
    proxy:
      require_service: [stripe]
      expose: 3000
      image: python:3.12
    run:
      command: [pytest, -q]
  ci:
    id: 9q1w2e3r4t5y6u7i8o9p0a1s2
    profile: dev                    # this environment lives on the dev plane
    ttl_minutes: 20
    boot: bundle
    proxy:
      require_service: ["stripe:1"]
      strict: true
    run:
      command: [pytest, -q, tests/integration]
```

Some services are not usable alone. A product service that signs in through
a family issuer — Google Calendar through Google Identity — is deployed with
that issuer in every sandbox, so the client's auth base URL resolves without
anyone knowing to ask for it. That has always been the control plane's
doing; what the CLI now does is say so. The service picker marks both sides,
`env create` names the issuer it added and whose sign-in it serves, and `up`
and `status` mark the twin nobody typed. The environment record still holds
only the services asked for, so one that gains an issuer later gains it in
environments defined before it.

`env create` writes `id`, and then only what a flag gave it: `ttl_minutes`,
`boot`, `snapshot`, `data`, `run.command`, and the `proxy:` block from `--image`,
`--require-service`, `--require-callback`, `--expose` and `--strict` (`env
create --help` shows the block each writes). No secrets and no sandbox ids go
in this file. The TTL's default, minimum and maximum are the control plane's,
not the CLI's: an environment with no `ttl_minutes` takes whatever it hands
out, and a number it will not accept is refused by it, with the bounds named
in the refusal.

Which environment is in use, most explicit first: `--env NAME|ID` →
`VERIS_ENV` → `use:` in `.veris/twin.local.yaml` → `default:` in the nearest
`.veris/twin.yaml` → the profile's `default_environment` →
`VERIS_ENVIRONMENT_ID` as a bare id. Layers never merge: a local `use:` hides
the project default entirely. `env use NAME` writes the folder-scoped override
into the gitignored `twin.local.yaml`, so a teammate's clone keeps the
committed default; `env use NAME --global` sets the profile-level default for
folders with no project file. A NAME resolves against the project file first,
then against the server by id or exact name; the shortened id `env list`
prints (`k3j2v0d8…`, or any prefix only one id begins with) is accepted at
both stages, and an ambiguous name or prefix lists the candidates.

| Command | Does |
|---|---|
| `env create [NAME] [--services a,b] [--from ID] [--ttl N] [--boot …] [--snapshot ID] [--data FILE] [--command 'cmd'] [--image TAG] [--require-service NAME[:N]] [--require-callback PATH[:N]] [--expose PORT] [--strict] [--default] [--force]` | Define a named environment. On a TTY it asks for the name and the services and nothing else. `--from` adopts an existing server environment instead of creating one. Unknown service names are refused with the catalog. The proxy flags write the `proxy:` block. Every setting left out is left out of the file, so `up` boots the bundle, seeds nothing, and takes the control plane's own TTL. |
| `env list` | Two blocks: configured (this project's, `★` default, `●` in use here) and available (every server environment the key can see, which config points at it, live sandboxes). |
| `env get [NAME\|ID]` | The resolved settings, where each came from, and the server record. |
| `env use [NAME\|ID] [--global]` | Choose for this folder, or the profile. A picker on a TTY when NAME is omitted. |
| `env delete NAME\|ID [--server] [--cascade]` | Drop the config. `--server` deletes the server environment too, refused while it has live sandboxes unless `--cascade` deletes them first. |

`veris init` is `env create --default` for a folder with no project file yet.

## Sandboxes

A sandbox is one disposable deployment of an environment, alive until its TTL.
`up`, `status` and `down` act on this folder's sandbox — the id is in
`.veris/twin.local.yaml` and never typed; `sandbox …` is the same set of
verbs for a sandbox named by `--id`. That split is what the root help is
grouped around. The three folder verbs answer under the group too
(`veris sandbox up`), spelled in full.

`up [NAME | --env NAME] [--ttl N] [--boot bundle|baseline|snapshot] [--snapshot
ID|NAME] [--callback-url URL] [--timeout 300s]` takes each setting from the
flag, then the environment config, then the defaults (boot bundle, and no TTL
of its own: a sandbox nobody gave one lives as long as the control plane says).
It writes the sandbox id to `.veris/twin.local.yaml` (0600, gitignored) as soon
as the control plane answers, then waits in two stages: until the sandbox is
`ready`, and then until every twin's `/veris/health` answers through the
public gateway, because the API measures ready from its own node and the
gateway can lag it by seconds. Only then is the sandbox routable and the
environment's `data:` files added. A sandbox that fails to boot is exit 1 with
the reason; one still on its way at the deadline is exit 4 and kept, since it
may yet come up, and `veris status` says. A folder already pointing at a
sandbox is warned that the old one keeps running until its TTL.

A twin verb that takes a name — `sandbox services get`, `sandbox services
manual` — can be typed without one. A sandbox holding a single twin uses it,
a terminal is asked which, and anything else is told the names the sandbox
actually has, written out as the commands to run.

`status` (and `sandbox get --id ID`) prints the sandbox's state, boot source
and expiry, then every twin with its status, env hint, URL and table counts.
`sandbox list [--env NAME | --all]` lists the in-use environment's sandboxes,
or every environment's; an environment that cannot be listed is a `!` line,
never silence. `sandbox reset` restores every twin to its boot seed and sets
the clock live; it is refused (409) for a sandbox booted from a snapshot or a
promoted baseline, because that world is an image, and the CLI prints the
allowed move: `veris down && veris up`.

### Working against it: `up --proxy`

`veris up --proxy` does not stop at a routable sandbox. It opens a shell
already routed at it, and returns when you leave.

```
$ veris up --proxy
✓ Up: 7hqz4m2n9c1v5x8b3k6t0r2p4 is this folder's sandbox (expires 16:04 EDT)
Answered by the sandbox, at the vendor's own hostname:
  api.stripe.com   → stripe
Not proxied — handed to the session as a variable:
  DATABASE_URL     → postgres
Every other host reaches its real destination. --strict refuses them instead.
Session in zsh, interception by proxy and CA variables
! Not enforced here: Java, static Go binaries, Apache HttpClient and aiohttp
  ignore those variables and reach the real vendor
  veris up --proxy --image <image> moves the redirect into the kernel, which
  covers every runtime.
veris: using sandbox 7hqz4m2n9c1v5x8b3k6t0r2p4 (this folder)
$ pytest -q
........                                                            [100%]
$ exit
veris: the sandbox received 7 request(s):
  stripe                       7
```

**There is nothing to source and no base URL to change.** Your code keeps its
production hostnames; the sandbox answers them. The banner says which
hostnames those are, which twins are handed over as variables instead of
intercepted, and what happens to everything else, so the mechanism is on
screen rather than taken on faith.

This is the shape for work `run` does not fit: a shell you keep, an app you
restart, a debugger you attach, an agent issuing many commands. Leaving the
session prints the receipt a run ends on — a session that never called the
sandbox says so.

`up --proxy` runs `veris run` for the session, so the two interception tiers
are exactly run's, and so is the choice between them:

| | Covers | Needs |
|---|---|---|
| `up --proxy` | curl, git, Python, Node, Go via `HTTP_PROXY`, .NET — anything that reads the proxy and CA variables | nothing |
| `up --proxy --image <img>` | **every runtime**: the redirect is `iptables` in the container's own network namespace, below every library | docker, and your code in an image |

The host tier is a *request*, not an enforcement, which is why it names what
it misses. There is no third option: a kernel redirect is only safe inside a
network namespace — on the host it would need root and would capture the whole
machine, and on macOS `iptables` does not exist at all. The container is the
namespace, which is why `--image` is the answer for "everything".

`down [--all]` deletes this folder's sandbox, or every sandbox of the in-use
environment after one confirmation. `sandbox delete --id ID` deletes any other.

Which sandbox a command means: `--sandbox` (`run`, `serve`) or `--id`
(`sandbox …`) → `VERIS_SANDBOX_ID` → the folder's pointer. The pointer records which environment its sandbox came from, and a
`run --env ci` at dev's sandbox is refused rather than take ci's command and
proxy settings to a world of dev's services.

## run

```sh
veris run [--sandbox <id>] [--env NAME] [--image <image>] [--cap-add <CAP>] [--require-service <n>] -- <cmd>
```

`run` resolves the project, the login and the folder for what the command line
left out, so the daily form is a bare `veris run`. The environment config
answers for the command after `--` (`run.command`), the requirements
(`proxy.require_service`, `proxy.require_callback`), what to expose
(`proxy.expose`), which image (`proxy.image`) and strict mode
(`proxy.strict`). Absence is what lets the file answer, not emptiness, so
`--expose 0` still wins over the file. `--env NAME` picks a config other than
the one in use; a name the project file does not know is refused. A refusal
that stems from a file setting names it (`proxy.expose in .veris/twin.yaml`)
rather than a flag you never typed.

Three ways to name where traffic goes. `--environment <id>` deploys a fresh
sandbox of that environment for this run and deletes it afterwards
(`--ttl-minutes` bounds its life if teardown never runs). `--sandbox <id>`
attaches to one that exists. With neither, and no `VERIS_SANDBOX_ID` or
`--config`, the run routes at this folder's pointer and says so on stderr.

### In a container (the default to reach for)

```sh
veris run --image your-image -v "$PWD:/work" -w /work -- pytest -q
```

Or with no command at all, letting the image's own `ENTRYPOINT` and `CMD` run,
which is what an application image is built to do. The proxy starts in its
own container, your image runs in a second one sharing that network
namespace, the output streams, your command's exit code propagates, and
everything is torn down. `-v`, `-e` and `-w` pass through to your container.

**Your image needs nothing** — no capability, no `iptables`, no entrypoint
change, no particular base. Distroless and scratch work. Every requirement
sits on the proxy container, which we build.

```
veris: using sandbox 7hqz4m2n9c1v5x8b3k6t0r2p4 (this folder)
veris: interception live in veris-proxy-35681
veris: sandbox ready sandbox_id=7hqz4m2n9c1v5x8b3k6t0r2p4
   7 customers, first: gus.thornton@example.com
veris: the sandbox received 1 request(s):
  stripe                       1
```

The `sandbox ready` line names the sandbox this run is routed at, so something
seeding state mid-run, or you diagnosing one, can address it. Your container
sees the same id as `VERIS_SANDBOX_ID`.

Your container runs with **every Linux capability dropped**. That is the
hardened default and it stays; what it costs is any entrypoint that switches
users — `su`, `gosu`, `service` — which fails with `cannot set groups:
Operation not permitted`. `--cap-add SETUID --cap-add SETGID` hands back
exactly the named capabilities and nothing else (`CHOWN`, `DAC_OVERRIDE`,
`FOWNER`, `NET_BIND_SERVICE` and the rest of the ordinary set are accepted;
`ALL` and `SYS_ADMIN` are refused, since they hand back the isolation itself).
Or skip the switch: build the image to run as the target `USER` and it needs
nothing. The proxy container's own capabilities are unaffected either way.

`--keep-proxy` leaves the proxy container running afterwards, to inspect it;
`--proxy-image` swaps the runner image (default
`ghcr.io/veris-ai/veris-cli:runner`). See
[`container/README.md`](container/README.md) for the two docker commands this
is doing for you, for running the proxy against an image you cannot restart,
and for adding it to an image you already have.

### Without a container

`veris run` without `--image` runs your command as a local child process with
proxy and CA environment variables set, which is a *request* rather than an
enforcement — a library that ignores those variables reaches the real vendor.
It does not cover Java, static Go binaries, Apache HttpClient or aiohttp; the
container path does. The flags that belong to the proxy container —
`--expose`, `--require-callback`, `--environment`, `--ttl-minutes`,
`--cap-add`, `--patch-bundled-cas` — are refused here with the reason, and
`--listen`, `--ca-dir` and `--java-truststore`, which describe a proxy in this
process, are refused with `--image`.

### The receipt and the exit code

`run` records every request the sandbox received, keyed by host, service and
matched path prefix, and prints the summary when the command finishes
(`--quiet` skips it). Sandbox control-plane requests — seeding, manuals, wire
traces — are counted apart on their own line and never satisfy a requirement:
they are usually the *harness* talking to the sandbox, not the code under
test.

An `--environment` run that sent the sandbox nothing exits 3 on its own —
deploying a sandbox for a suite that never called it is a failure, not a
pass. `--require-service stripe[:count]` and `--require-host host[:count]`
sharpen that into per-service assertions and take over the verdict entirely.
Attaching, with `--sandbox` or the folder's pointer, asserts nothing by
default: the receipt is still printed, so a run that sent nothing says so, but
the exit code stays your command's — which is why `proxy.require_service` in
the project file is worth setting.

`run` exits with the command's own status, or 3 if a requirement went unmet,
or 4 if the outcome is indeterminate. `--strict` blocks unmapped hosts (see
below). `--route service=host[/prefix]`, for this run, replaces the routes the
control plane served for one service or supplies a hostname it served none
for.

### Receiving webhooks

The proxy routes your code OUT to the sandbox. A webhook comes back IN, and a
hosted sandbox cannot reach an app on your laptop. `--expose` opens a tunnel
for that direction and registers it with the sandbox:

```sh
veris run --image your-image --expose 3000 --require-callback /hooks/stripe
```

The port is the one your app listens on. Your image starts only after the proxy
reports ready. A new tunnel hostname may take time to resolve from the sandbox;
the proxy retries DNS failures for up to one minute before allowing the app to
start, and refuses startup if resolution still fails. This checks the sandbox's
resolver, not the laptop's. The registration probe runs before anything is
listening; the proxy waits for your port to open and re-probes, and the verdict
you read is the one taken then. Your app is handed `VERIS_PUBLIC_URL` and
registers that with the vendor itself — through the vendor's own API, because
that registration call is the code path that ships.

```
veris: callbacks arrive at https://odd-forest-1a2b.trycloudflare.com
veris: callbacks registered  via=stripe
...
veris: your app received 2 callback(s):
  POST   /hooks/stripe                2 -> 200
```

`--require-callback /hooks/stripe[:count]` exits 3 if nothing arrived (`*`
for any path). Without it a webhook suite that received nothing still passes,
which is the same failure the egress receipt exists to catch.

`--receipt run.json` saves both ledgers and the verdict. `engine.callbacks`
records the inbound method, path, HTTP status and count, including rejected
callbacks; `sandbox.deliveries` records the sandbox's outbound attempts. Check
the application's signature and processing assertions alongside those counts.

A quick tunnel needs no Cloudflare account and mints a new hostname each run.
`--expose-token` (or `VERIS_TUNNEL_TOKEN`, plus `--expose-hostname`) uses a
named tunnel instead, for a stable URL. In Cloudflare, configure that hostname's
service URL as `http://127.0.0.1:18444`: Veris's callback recorder inside the
network namespace where cloudflared runs. The recorder forwards to `--expose`
and records the delivery. Token tunnels use Cloudflare's remote configuration;
`--url` cannot override it. Use a dedicated tunnel/connector for this run so
another connector cannot receive its callbacks. One named-tunnel run can use
port 18444 per network namespace; separate runner containers have separate
namespaces. The CLI waits up to a minute for a fresh sandbox probe through this
recorder and explains the required service URL if the route is wrong. An app
that has not started listening yet does not prevent recorder readiness.

If your app runs on the HOST while the
proxy is in a container, add `--expose-host host.docker.internal` — loopback
there is the container's own.

When you are receiving callbacks, a sandbox per run is not just simpler but
safer, for a reason worth stating plainly: the callback destination is a
sandbox-wide singleton. Two concurrent runs sharing one sandbox overwrite each
other's callback URL, and the first run's webhooks are then delivered to the
second run's app — silently, with neither able to notice. `--environment`
removes that entirely, and removes the registration window too: the tunnel
needs only a local port, so it opens first and its URL is handed over at
creation; the sandbox is never alive without knowing where to deliver.
Attaching to an existing sandbox still registers, by PATCH, and warns when it
replaces someone else's URL. The URL is a capability, in either mode: anyone
holding it can POST to your app.

### Unproxied services: handed over, not proxied

A sandbox can hold services the proxy has no hostname to intercept for: a
Postgres service's `url` is a connection string, a wire protocol this proxy
does not speak, and a data-plane twin such as yente answers on a locally run
instance that no client calls over the internet, so no vendor hostname is
measured for it. Interception would be the wrong tool anyway: client code
already reads its database DSN or base URL from an environment variable in
production, so configuration through the environment IS the code path that
ships. Since the control plane is now the only source of vendor hostnames
([where they come from](#where-the-hostnames-come-from)), a twin it serves
none for lands here too — that one is not by design, and `veris doctor` names
it on its vendor-hostnames line.

`run` hands each such service's URL to your command under the exact variable
the platform names for it (its `env_hint` — `DATABASE_URL` for Postgres,
`YENTE_API_BASE` for yente), in every tier: the local child process, the
workload container, and `serve --write-env` output, including trust-only
mode. The run says so per variable — `veris: yente: not proxied; handed
YENTE_API_BASE=…` — so an unproxied twin reads as the deliberate handoff it
is, and a `--require-service` on it is judged on the sandbox's own ledger,
the only one its traffic ever reaches. An explicit `-e DATABASE_URL=...` of
your own is never overwritten, exactly as `docker run` precedence has it.

## serve and check

| Command | Purpose |
|---|---|
| `serve` | The proxy as a process: the container image's entrypoint, a supervisor's child, with `--write-env` and `--ready-file` as its handoff. At a keyboard you want `veris up --proxy`. |
| `check` | Assert a live proxy belongs to THIS run. Exit 2 if not. |

```sh
veris serve                                       # this folder's sandbox and login
veris serve --sandbox 7hqz4m2n9c1v5x8b3k6t0r2p4 --expose 3000
veris serve --environment k3j2v0d8p1q7x9r2m5n8b4c6a --expose 3000
```

With no `--sandbox`, `--config` or `--environment`, `serve` routes at this
folder's sandbox and uses this folder's login, exactly as `run` does — so it
needs no id and no `VERIS_API_KEY` on a machine that has logged in.

Ingress belongs to the session rather than to one command, so `serve` owns
it; `run --image` takes the same `--expose*` and `--require-callback` flags
and forwards them to the proxy container. `serve --environment` deploys a
sandbox for the session the way `run --environment` does for a run, with
`--ttl-minutes` (default 60) as the leak bound.

`serve --write-env FILE` and `serve --ready-file FILE` are how a supervisor
picks the proxy up: the environment as sourceable POSIX exports
(`--env-format docker` for `docker run --env-file`), and an edge-triggered
marker written last, so its existence means every listener is bound and the
environment is complete. `--write-env` records the **bound** address, so
`--listen :0` works. `--env-trust-only` emits only the CA variables, for a
tier where the kernel already does the routing. `serve --print-routes` shows
what a sandbox id resolves to without starting anything.

`check [--expect-canary <token>] [--any-run] [--proxy <url>] [--timeout 5s]`
asserts on a per-run canary token *before* your tests run, reading
`VERIS_PROXY_URL` and `VERIS_CANARY` from the environment `serve` wrote. It
fails if the proxy is unreachable, if it is not a Veris proxy, or if it belongs
to a different run — a proxy left running from an earlier run, pointing at a
different sandbox, would otherwise let tests pass against the wrong simulated
data. A canary always exists: the proxy mints one per process when the config
names none, so the assertion never quietly weakens into a liveness probe.
`--any-run` accepts any live Veris proxy.

The receipt answers the question the canary cannot: interception was live, but
did this run actually *use* it? A suite that quietly stopped calling its
dependency passes the canary check and produces an empty receipt. Without
both, "interception silently did not happen" and "everything worked" look
identical.

## doctor

`veris doctor [--env NAME]` is the one screen that answers "why is my first
run failing". Every check is one line — `✓` passed, `!` worth knowing, `✗`
will fail a run — ordered the way a run depends on them: the binary, the
login, the plane, the gateway, the vendor hostnames it serves, docker, the
project file, the environment, the sandbox. Nothing is changed; the `→` lines
name the command that would.

```
$ veris doctor
✓ veris v0.9.0 (darwin/arm64)
✓ Logged in: Acme via profile 'default' (vsk_mi4pa0uo…)
✓ Control plane https://svc.api.veris.ai reachable (status ok)
✓ Vendor hostnames served for 30 of 32 catalog twins; none for postgres, yente (not intercepted; handed to the command instead)
! docker not on PATH — host tier works; --image (container tier) will not
✓ Project file /Users/victor/src/checkout-svc/.veris/twin.yaml (2 environments, default 'dev')
✓ Environment dev (k3j2v0d8…) reachable; services: stripe, postgres
```

It exits 1 when any check failed, and `--json` puts the same checks on stdout.
`veris version` prints the binary's version and, when a plane is resolved and
answers within 3 s, the plane's on a second line.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | usage or configuration error, API refusal, a sandbox that failed to boot, a login denied or expired, a declined confirmation |
| 2 | `check` failed: no proxy, not ours, or a different run |
| 3 | the run did not call a service it was required to call (`--require-service`, `--require-host`, `--require-callback`, or an empty receipt on an `--environment` run) |
| 4 | the run's outcome is indeterminate; `up` timed out with the sandbox kept |
| n | otherwise, whatever the command under `run` exited with |

## Environment variables

| Variable | Purpose |
|---|---|
| `VERIS_API_KEY` | Beats the profile on every command; the profile's key is then never read. Never written to disk. |
| `VERIS_API_BASE` | The control plane, when not `https://svc.api.veris.ai`. |
| `VERIS_PROFILE` | Which login to use, below `--profile`. |
| `VERIS_ENV` | Environment name or id, below `--env`. |
| `VERIS_ENVIRONMENT_ID` | A bare environment id, honoured last in the environment chain; `serve --environment` defaults to it. |
| `VERIS_SANDBOX_ID` | Sandbox to route at, when no flag names one and above the folder's pointer. |
| `VERIS_PROXY_CONFIG` | Path to a proxy config file. |
| `VERIS_TUNNEL_TOKEN` | cloudflared named-tunnel token, for `--expose` with a stable hostname. |
| `VERIS_PROXY_URL`, `VERIS_CANARY` | Written by `serve --write-env`; read by `check`. |
| `VERIS_PUBLIC_URL` | Handed to your app under `--expose`: the URL callbacks arrive at. |

`--api-key`, `--api-base`, `--sandbox` and `--config` override the matching
variable. Precedence for every value is the most explicit source that says
anything: flag → environment variable → `.veris/twin.local.yaml` →
`.veris/twin.yaml` → profile → built-in default.

## Config

Naming a sandbox is usually all you need — the routes come from the control
plane. A config file is for pinning them yourself. JSON rather than YAML keeps
the wire format unambiguous. See [`examples/proxy.json`](examples/proxy.json).

```json
{
  "version": 1,
  "listen": "127.0.0.1:8080",
  "sandbox_id": "7hqz4m2n9c1v5x8b3k6t0r2p4",
  "mode": "strict",
  "upstream": { "base_url": "https://sandbox.veris.ai" },
  "services": [
    { "name": "stripe", "hosts": ["api.stripe.com", "*.stripe.com"] },
    { "name": "google-calendar", "hosts": ["www.googleapis.com"],
      "paths": ["/calendar/v3"] },
    { "name": "google-drive", "hosts": ["www.googleapis.com"],
      "paths": ["/drive/v3", "/upload/drive/v3"] }
  ]
}
```

`run` and `serve` take the same routing flags, most explicit first:
`--config <file>` · `--sandbox <id>` · `$VERIS_PROXY_CONFIG` ·
`$VERIS_SANDBOX_ID`. The layers never merge.

Host matching is exact or a single leading `*.` wildcard. Exact always beats
wildcard, so `api.stripe.com` can route differently from `*.stripe.com`.

`paths` narrows an entry to request paths under a prefix, which some vendors
require: Google fronts Calendar, Drive and its identity endpoints on
`www.googleapis.com`, told apart only by prefix. Prefixes match on a segment
boundary, so `/userinfo` claims `/userinfo/v2/me` and not `/userinfoXYZ`. Host
specificity outranks prefix length; within one host the longer prefix wins; an
entry with no `paths` claims the whole host and loses to any explicit prefix on
it. Two services claiming the same host *and* prefix is rejected at load rather
than resolved by declaration order.

`allow_passthrough` accepts the preset `"@build"`, which expands to the
package registries a build tool needs (Maven Central, Gradle, npm, PyPI, Go,
crates.io, RubyGems, NuGet, Packagist). Without it, running tests behind the
strict proxy means resolving dependencies with interception off and then
forcing the build tool offline; with it, dependency traffic flows around the
proxy while the strict-mode guarantee stays auditable — these exact hosts and
nothing else. The list is first-party registry hosts only; a project on a
private registry adds its own host next to the preset.

An unresolved service upstream is derived as
`{base_url}/s/{sandbox_id}/{service}`.

### Where the hostnames come from

You do not have to write the `hosts` and `paths` above. They come from the
control plane, with every sandbox and from `GET /v1/services`, and from
nowhere else. They are **generated** there from each service's measured
backend — the host the simulation was actually driven against — so a service
added to the platform is routable the day it lands, with no release of this
binary.

That matters because the facts are not guessable. A hand-written table put
Google's `/tokeninfo` on `www.googleapis.com`; the measured record puts it on
`oauth2.googleapis.com`, where Google actually serves it, so a client's token
introspection would have been routed at a service that does not answer there.
Google's identity surface alone spans four hostnames, and three Google services
share a fourth.

The binary keeps no copy of the hostnames. A second copy is a second chance to
be wrong, and one that had gone stale would reroute traffic silently. A
service the control plane serves no hostname for is therefore not intercepted
at all: its URL is handed to your command under its `env_hint`, the same
handoff a Postgres DSN gets, and the run says so per service. A service with
no `env_hint` either is reported as out of reach instead. `veris doctor` names
every catalog twin the plane serves no hostname for — `postgres` and `yente`
are that by design, anything else there is a measurement that has not landed —
and `--route <service>=<host>[/prefix]` supplies one for a single run.

## Two design decisions worth knowing

### Only provisioned services are rerouted

A host with no matching service reaches its real destination. Telemetry,
package registries, an internal API and anything else the code under test talks
to behave exactly as they always did, so pointing a project at a sandbox is one
command rather than a configuration project.

`--strict` (or `"mode": "strict"`, or `proxy.strict` in the project file)
blocks unmapped hosts with a `421 Misdirected Request` and an actionable
error, for a run that has to prove the code under test reached nothing but the
sandbox. 421 because a missing route is permanent for the life of the run: 5xx
sits in the retry set of every HTTP client, so a blocked request used to spend
the caller's whole retry budget in backoff before failing anyway. That
guarantee is real, but it is not the only way to get it: the receipt reports
what the sandbox actually received, so a suite quietly talking to the real
vendor is visible without having to forbid every host nobody thought to list.

### The receipt makes a silent no-op impossible

Two mechanisms, answering two different questions: `check` proves the proxy in
front of the suite is this run's, and the receipt proves the suite used it.
Either alone leaves a way for a green run to have tested nothing.

## HTTP/2

All tiers negotiate h2 by ALPN, and fall back to HTTP/1.1 for a client that
does not offer it. The leg from the proxy to the sandbox asks for h2 too.

This is not a detail. Google, Stripe and most large vendors serve HTTP/2, so a
client that negotiates h2 in production and HTTP/1.1 here is exercising a
different transport than the one that ships — different multiplexing, different
header handling, and a different set of code paths inside its own HTTP library.

## Two tiers of interception

### Kernel redirect, in a container — the default

`iptables REDIRECT` moves the traffic in the kernel, below every library, so
nothing in the process under test has to honour anything. Needs
`--cap-add=NET_ADMIN`. Two arrangements, and which one you want depends on
whether you control the image:

- **Your image joins the proxy's network namespace** (`--network
  container:veris-proxy`) — the one to reach for, and what `run --image`
  does. Every requirement lands on our container rather than yours: it is
  root, it has iptables, it drops itself. Yours needs no capability, no
  iptables, no entrypoint change and no particular base image, so distroless
  and scratch work.
- **One container, proxy inside.** Simpler to operate, and the trade is that
  those requirements move onto your image, which must also start as root.
  Missing any of them the entrypoint refuses rather than silently degrading.

`serve --transparent` stands itself up when it starts as root on Linux: it
installs the redirect, puts the CA in the system trust store, and drops to an
unprivileged uid before serving. So an image can run the binary directly and
needs no entrypoint script from us. `--redirect-external` says something else
installed the redirect.

The proxy runs as a dedicated uid (14741, `--proxy-uid`) inside that
container, because the redirect exempts its own upstream calls by uid and one
rule cannot tell two processes with one uid apart. The entrypoint refuses to
start if your command would share it.

See [`container/README.md`](container/README.md) for both arrangements, and for
composing with an entrypoint you already have.

### Explicit proxy, on the host — the fallback

`run` sets the full matrix of proxy and CA variables on the command it starts,
and `serve --write-env` writes the same set to a file. There is no standard for
any of them, so each runtime needs its own:

| Runtime | Proxy | CA |
|---|---|---|
| Python requests / httpx | env | `REQUESTS_CA_BUNDLE` / `SSL_CERT_FILE` |
| Go | env | `SSL_CERT_FILE` (Linux only) |
| Node | needs `--use-env-proxy` | `NODE_EXTRA_CA_CERTS` |
| .NET | env | `SSL_CERT_FILE` (Linux only) |
| Java | `JAVA_TOOL_OPTIONS` | JKS truststore, not a PEM |

Java deserves its own paragraph because it reads none of the usual variables
and wants a JKS rather than a PEM. `run` builds one when it finds a JDK —
copying the JDK's own cacerts and adding the Veris CA, never replacing it,
since a store holding only our CA would break every other TLS connection in the
JVM — and emits `JAVA_TOOL_OPTIONS` with the `-D` proxy, `nonProxyHosts` and
truststore flags, which every JVM including Gradle and Maven test forks picks
up. `--java-truststore` and `--java-truststore-pass` name one you built.

An app that loads its own keystore from disk never consults the JVM default
truststore. Add the CA to its keystore yourself:

```sh
keytool -importcert -noprompt -trustcacerts -alias veris-local-ca \
  -file ~/.veris/ca/veris-ca.pem -keystore your-keystore.p12 -storepass ...
```

`run` prints what it cannot cover to stderr rather than letting you discover it
as a mystery TLS failure. Four cases are genuinely out of reach: Go on macOS
ignores `SSL_CERT_FILE` and verifies through Security.framework; Apache
HttpClient built with `createDefault()` ignores the JVM proxy properties;
`aiohttp` ignores proxy variables without `trust_env=True`; and the Stripe
Python and Ruby SDKs ship their own CA bundle. The container tier covers the
*routing* half of all four; for the Stripe case the *trust* half stays
in-process, which is what `--patch-bundled-cas` and the trust-rejection
diagnostics below exist for.

## Certificates

The CA is generated on first run into `~/.veris/ca`, reused thereafter, and the
key is written `0600`. Leaves are minted per host and cached in memory.

Two details that are easy to get wrong and are covered by tests: leaves are
served as **leaf + CA**, since Node and anything on OpenSSL reject a bare leaf
with `UNABLE_TO_VERIFY_LEAF_SIGNATURE`; and every leaf carries a SAN, since a
certificate with only a CN is rejected by every modern client.

### SDKs that bundle their own CA

Kernel-level routing reaches every runtime, but *trust* is still decided
inside the process, and an SDK that ships its own CA file and hands it
straight to the TLS layer — stripe-python and stripe-ruby, older botocore,
httplib2 — reads none of the trust environment and refuses the minted leaf.
Three mechanisms close that gap, in the order to reach for them:

1. **Documented overrides in the environment.** Tools with a private bundle
   and an official override read it from `veris.env` already: gRPC
   (`GRPC_DEFAULT_SSL_ROOTS_FILE_PATH`), Bundler, Composer, Hex, Julia, Nix,
   Perl LWP, gcloud. Nothing to do.
2. **`run --image ... --patch-bundled-cas`**, the default for an SDK that
   bundles its own CA. Scans the image
   and your `-v` mounts for known bundled CA files (certifi, pip's vendored
   certifi, botocore, Stripe's Python and Ruby layouts, httplib2), appends
   the Veris CA to a copy of each, and bind-mounts the copy read-only over
   its exact path. The SDK keeps loading its own bundle through its own code
   path; the file just carries one more root. A bundle it cannot read or
   patch fails the run loudly, and one line per overlay says what happened.
3. **The diagnostics name the next action, whatever the SDK.** Detection is
   handshake-level and language-agnostic: any client in any runtime that
   refuses the minted certificate is recorded per host, and a mapped host
   whose handshakes were all refused with zero completed vendor-surface
   requests fails the run with exit 3. Every refusal message ends in a
   prescription, not a pointer: without `--patch-bundled-cas`, it says to
   re-run with it; with the flag on, the scan's report of CA-bundle-shaped
   files it does *not* know names the exact file to over-mount by hand — and
   when no such file exists anywhere in the image or mounts, the message says
   this is real certificate pinning and to stop, because no retry changes it.

What none of this covers is real pinning — an SDK comparing SPKI or
certificate hashes after chain validation (OkHttp `CertificatePinner`, curl
`--pinnedpubkey`, aiohttp `fingerprint=`). No added root can satisfy that;
it is a boundary, not a configuration problem.

## Development

```sh
make test    # go test -race
make lint    # gofmt + go vet
make build   # a binary for this machine (bin/veris)
make dist    # static binaries for 5 platforms
make e2e     # against real curl, Python and Node clients
```

The e2e script matters because the Go tests exercise the proxy through Go's own
TLS stack, which is more forgiving than OpenSSL's. CI runs the unit tests, the
race detector and every cross-build on any PR.

### Releasing

**Nobody picks the number and nobody pushes the tag.** `ci` passing on `main`
is the trigger; the commit subjects since the last tag decide the bump; the
release workflow cuts the tag, attaches the `make dist` binaries the installer
downloads, and publishes the runner image.

Every commit on `main` is a squashed pull request whose title this repo already
requires to be `type(scope): subject`, so the history is a conventional-commit
log without anyone maintaining it as one:

| Commits since the last tag | Result |
|---|---|
| `feat` | minor |
| `fix`, `perf` | patch |
| `!` before the colon, or `BREAKING CHANGE:` in the body | see below |
| only `docs`, `chore`, `ci`, `test`, `refactor`, `style`, `build` | **no release** |

The last row is deliberate. Those change nothing a user of the binary can
observe, and a release nobody can tell apart from the last one is noise in the
changelog and a download that gains its taker nothing.

**Before 1.0**, which is where this is, a breaking change moves the MINOR.
`0.y.z` is semver's own place for a public API that is not yet stable, and
going to `1.0.0` is a claim about stability that a script reading commit
subjects has no business making. Ask for it by hand: run the release workflow
with `bump: major`, which is also the way to force any bump the commits did
not earn.

`scripts/next-version.sh` decides it and runs the same on a laptop, so you can
see what the next merge would cut before you make it:

```sh
scripts/next-version.sh          # release=… bump=… previous=… version=…
bash scripts/next-version-test.sh # the rules, held to examples
```

Pushing a tag by hand still works and still publishes — it is the escape hatch,
not the path.

## Licence

MIT. Built on [elazarl/goproxy](https://github.com/elazarl/goproxy) (BSD-3).

### Reading complete sandbox tables

`veris sandbox data get NAME TABLE` reads one page (20 rows by default).
`--json` preserves the row-array format and reports partial reads on stderr.
Use `--offset N --limit N` for another page, or `--all --json` to collect every
page before printing the array. `--limit` is the page size, at most 1000.
Stop writers while collecting pages: this is not an atomic snapshot. A changed
total, stalled page, or failed request fails the command without emitting a
partial JSON array. Filter the complete array locally to count rows from a run.

### Import file bodies

`veris sandbox files import google-drive ./pdfs --owner OWNER --prefix Corpus`
streams a local directory through the service's existing `/veris/files` control
surface. For GCS sources, first use `gcloud storage rsync gs://BUCKET/pdfs ./pdfs`.
The command hashes every file, stages ZIP batches (128 MiB payload by default),
and streams an individual file raw when it exceeds the batch budget. Files
larger than 1 GiB, symlinks, and special files are refused.

Use `--checkpoint PATH --resume` to resume the exact source and destination,
skipping acknowledged batches. A changed manifest is refused. A disconnected
POST may have committed: its pending batch is retained and automatic replay is
refused until the operator reconciles the listed files with the service. Each
batch is atomic; the whole directory is not. Repeated paths can create revisions.
A killed importer may leave a `.lock` beside its checkpoint; remove that lock
only after confirming the importer is no longer running. Checkpoints contain
service capability URLs and should remain private. `--json` emits a receipt
with SHA-256 hashes and acknowledged byte/file totals; progress goes to stderr.

After upload, promote with `--keep-source` or create a snapshot, boot a separate
sandbox and verify its file downloads before deleting the source. The command
does not promote automatically and does not claim a successful restore.
