# Changelog

What changed in each tug release, newest first, and what to do when you
upgrade. tug follows [semantic versioning](https://semver.org): until 1.0, a
minor release may change behaviour, and its upgrade notes say how.

Releases from 0.3.1 on are on
[GitHub](https://github.com/paingha/tugcli/releases), with signed checksums
and SBOMs. The releases before 0.3.1 are no longer there; their entries stay
here as a record of what changed. The
[user guide](https://paingha.github.io/tugcli/) covers each command in full.

## 0.3.1

Released 22 November 2026.

Release builds that know they are releases, and unlock hints from tugd and
the TUI that match your team's policy. Releases before this one are no
longer on GitHub, so install 0.3.1 rather than an older version.

### Upgrading from 0.3.0

Install 0.3.1 the way you installed 0.3.0. Your `config.yaml`, catalog,
sessions, pins, credentials and plugins work as they did, and the
first-party plugins are unchanged, so there is nothing to reinstall. What to
know:

- **`tug version` prints `v0.3.1`.** The 0.3.0 binaries printed `0.3.0`,
  without the `v`, and so counted as development builds.
- **Plugins' minimum tug version is checked again.** A 0.3.0 release build
  skipped `spec.minTugVersion`; 0.3.1 refuses a plugin that needs a newer
  tug and says to upgrade. `tug plugin init` pins new plugins to the SDK of
  the tug that made them again, too.

### Fixes

- tugd's [local API proxy](https://paingha.github.io/tugcli/docs/guides/proxy)
  and the TUI suggested `tug unlock 15m` under a team policy that allows
  shorter unlocks, which `tug unlock` then refused. They now suggest the
  unlock the policy allows, as tug's commands do: when a write is refused
  through the proxy or in the TUI, and when the TUI connects to a read-only
  cluster.
- Releases are built with Go 1.26.11 instead of 1.26.0, whose Windows I/O
  could corrupt a goroutine's stack and crash tug while it streamed logs
  ([golang/go#77975](https://github.com/golang/go/issues/77975)).

### Docs

- A new page, [How tug compares](https://paingha.github.io/tugcli/docs/compare),
  sets tug beside kubectx and kubens, kubie, kubeswitch and k9s: what tug
  adds over each, and when to choose the other tool.
- The docs home page names the latest release. It had announced
  0.1.0-alpha.1 since that release.
- The installation guide says what to do when `tug version` shows an older
  version than the one you installed: another `tug` comes first on your
  `PATH`, often one from `go install`.

## 0.3.0

Released 22 November 2026.

tug's settings without editing `config.yaml`, `tug why --deep` turned on in
one step, and fixes for objects tug missed after their kinds were installed.

### Upgrading from 0.2.0

Install 0.3.0 the way you installed 0.2.0. Your `config.yaml`, catalog,
sessions, pins, credentials and plugins work as they did, and the
first-party plugins are unchanged, so there is nothing to reinstall. What to
know:

- **`tug settings` now belongs to tug.** tug runs any command it doesn't
  have as kubectl, so a kubectl plugin named `settings` now needs
  `kubectl settings`.
- **Hints suggest the unlock your team allows.** Under a team policy that
  limits unlocks to less than 15 minutes, `tug profile up`,
  `tug auth rotate` and the refusal of a write run with `tug` now suggest an
  unlock of that limit, as `tug connect` already did, instead of a
  `tug unlock 15m` that would be refused.

### Settings

- **`tug settings list|get|set|unset|add|remove|path`** reads and changes
  `config.yaml` for you, changing only what you name and keeping the rest of
  the file and its comments. `list` shows every setting, the value tug uses
  and where it comes from: the file, a variable such as
  `TUG_KEYRING_BACKEND`, or a default. Lists and maps change an item at a
  time: `tug settings add guard.readOnlyLabels team=payments`,
  `tug settings add plugins.indexes name=acme url=... publicKey=...`,
  `tug settings unset plugins.pins.digitalocean`. Every setting except
  `teams`, which `tug team` owns, can be changed this way.
  [Reference](https://paingha.github.io/tugcli/docs/reference/commands/other#tug-settings).
- tug refuses a value it could not use, and shows a change that makes it
  trust something or protect less before making it: a plugin index, a plugin
  outside the sandbox, a cluster no longer read-only. Without a terminal
  that needs `--yes`. Secrets are never settings; a key that looks like one
  is answered with where secrets go.
- The first label set you add to `guard.readOnlyLabels` keeps `env=prod`
  beside it, so production still opens read-only.
- Errors and help that told you to edit lists in `config.yaml` now give the
  `tug settings add` command that does it.

### AI explanations

- **`tug intelligence enable [provider]`** turns `tug why --deep` on in one
  step: it sets the provider, takes `--model` and `--base-url`, and asks for
  the API key on a terminal when none is stored. A setup that cannot work is
  refused before anything is written. **`tug intelligence disable`** turns
  it off and keeps the other settings, the ledger and the keys.
  [Guide](https://paingha.github.io/tugcli/docs/guides/ai-explanations#turning-it-on).
- `--deep` counts tug's own read-only commands, such as
  `tug reconcile explain` and `tug check`, as next steps that only read,
  instead of moving them to the changes that need approval.

### Fixes

- `tug tree`, `why`, `watch`, `reconcile` and `tug support bundle` missed
  objects whose CRD was installed after tug's discovery cache was filled,
  for up to six hours. They now read discovery afresh once per command.
- `tug gitops status` said Flux or Argo CD wasn't installed when it was
  installed after the discovery cache was filled.
- `tug reconcile status kube-controller-manager` and other static pods found
  by their bare name failed with "nothing in any namespace matches" on kind
  and kubeadm clusters.
- In a colour terminal, a coloured cell, such as a red `prod` in `tug ls`,
  pushed the columns after it out of line.
- Indirect dependencies used to verify plugin signatures were updated for
  security fixes: go-jose 4.1.9, gRPC 1.83.2 and OpenTelemetry 1.47.0.

### Docs

- The [user guide](https://paingha.github.io/tugcli/) shows real output for
  tug's commands, and a new
  [TUI guide](https://paingha.github.io/tugcli/docs/guides/tui) has
  screenshots. The README is rewritten for people using tug.

## 0.2.0

Released 21 November 2026.

Most of the road to 1.0: team catalogs with safety policies, profiles, policy
checks, controller and GitOps diagnostics, live watches, support bundles,
diffs and drift, an API and CRD explorer, shortcuts, optional AI
explanations, a sturdier tugd and stricter plugin governance.

### Upgrading from 0.1.x

Install 0.2.0 the way you installed 0.1.x. Nothing has to change: your
`config.yaml`, catalog, sessions, pins, credentials and plugins work as they
did. Every setting below is new; none was renamed or removed. What to know:

- **Restart tugd.** A tugd started by 0.1.x keeps running after the upgrade,
  without this release's fixes or port-forward groups. Run `tug daemon stop`;
  tug starts the new one when it next needs it. Named port-forwards stop with
  it, and the next command in each proxied terminal fails once, saying that
  running it again asks for a new token.
- **Open a new shell for the new prompt.** New shells get a red badge on
  production clusters and an unlock countdown that ends on time without
  starting a tug process. Shells started before the upgrade keep the old
  prompt, which still works.
- **`tug diff` is now tug's own command.** 0.1.x handed it to `kubectl diff`.
  It still takes `-f` and `--server-side` and exits as `kubectl diff` does: 0
  when nothing differs, 1 when something does, 2 on an error. The output now
  names the field manager that owns each field that differs, and kubectl's
  other diff flags, such as `-k` and `--prune`, are not accepted. Run
  `kubectl diff` for kubectl's own.
- **New command names now belong to tug.** tug runs any command it
  doesn't have as kubectl, so `tug team`, `profile`, `check`, `reconcile`,
  `watch`, `support`, `diff`, `drift`, `api`, `crd`, `gitops`, `alias`,
  `bookmark`, `pick`, `open` and `intelligence` now run tug's command instead
  of a kubectl plugin of the same name. kubectl's `api-resources` and
  `api-versions` still pass through.
- **Some tables have new columns.** `tug audit` shows EVENT, DETAIL and
  RESULT, `tug plugin ls` shows PINNED, and `tug pf ls` shows PROFILE.
  Scripts should read `-o json`. JSON and YAML only gained fields: `aliases`
  and `team` in `tug ls`, `env`, `production` and `profile` in `tug status`,
  and new events and fields in `tug audit`.
- **The audit log records more:** refused writes, the outcome of each write,
  session start and end, and unlock, lock and unlock expiry. Older entries
  read as before.
- **Reinstall the first-party plugins to update them.** They are now 0.1.1,
  built with the new SDK. Install the 0.1.1 archive from this release as you
  installed 0.1.0; it goes in beside 0.1.0, and tug runs the highest
  installed version unless the plugin is pinned.
- **Plugin checks are stricter.** A plugin version must be a single path
  element: letters, digits, dots, dashes, underscores and pluses. Control
  characters in what a plugin prints are removed. A manifest may ask for
  limits now (`spec.limits`), and a timeout it asks for is capped at five
  minutes unless `plugins.limits.timeout` says otherwise. A plugin that asks
  for no limits runs as before.
- **Upgrade before your plugin index revokes or rotates.** 0.1.x refuses an
  index that uses `revoked` or `supersededKeys` rather than install a plugin
  it was told to refuse.

Some new features ask first or refuse, but only once you use them:

- A team catalog asks before it is trusted, and again for each new version;
  without a terminal it needs `--yes`. Under a team policy with `maxUnlock`,
  a longer `tug unlock` is refused, a plain `tug unlock` lasts at most the
  limit, and `tug connect --read-write` is refused. The team's labels decide
  which policies apply, so a label set by hand cannot take a cluster out of
  one.
- A profile can make a session read-only but never read-write, and one that
  asks for read-write is refused when it is saved.
- A pinned plugin (`plugins.pins`) installs only at its pinned version, and
  tug runs that version.
- A revoked plugin archive is refused, and `tug plugin ls` warns about one
  already installed.
- `tug why --deep` is off until `intelligence.enabled` is set. It shows what
  it will send and what it may cost, then asks; without a terminal it needs
  `--yes`. A call that could pass `monthlySpendCapUSD` is refused.

New settings in `config.yaml`, described in the
[configuration reference](https://paingha.github.io/tugcli/docs/reference/configuration):
`teams`, `policy.packs` and `policy.default`, `intelligence`, `plugins.pins`
and `plugins.limits`. New files sit in tug's own directories, listed in the
[files reference](https://paingha.github.io/tugcli/docs/reference/files):
`profiles/`, `aliases.yaml`, `bookmarks.yaml`, `teams/`,
`plugin-index-keys.yaml` and a receipt beside each plugin installed from now
on. The catalog also records which team, at which version, an entry came
from. A 0.1.x catalog is read as it is.

### Teams and safety

- **Team catalogs** (`tug team add|sync|ls|show|remove`): a team publishes
  signed names, aliases and labels for its clusters, provider account
  settings, approved plugin indexes and safety policies. A catalog never
  carries credentials: each person reaches every cluster through their own
  sign-ins. tug checks the ed25519 signature, refuses an older version or a
  changed one that kept its version, and works offline from the last good
  copy. Publishers sign with `tug team keygen|sign|verify`.
  [Guide](https://paingha.github.io/tugcli/docs/guides/teams).
- **Safety policies** in a team catalog make matching clusters read-only and
  limit how long an unlock lasts. The stricter of a policy and
  `guard.readOnlyLabels` always wins, in `tug connect`, pins, the TUI,
  `tug auth rotate` and, through tugd, kubectl, helm and k9s.

### Profiles and the prompt

- **Profiles** (`tug profile use|up|down|ls|show|edit|run`): a reviewable file
  that bundles a cluster, namespace, safety mode, command aliases and named
  port-forwards. `up` also starts the forwards and `down` stops them.
  [Guide](https://paingha.github.io/tugcli/docs/guides/profiles).
- **Port-forward groups** (`tug pf profile up|down|ls`) check every local port
  first and start nothing if any is taken.
- **The prompt** shows a production badge in every mode and counts an unlock
  down on time in zsh, bash, fish and PowerShell. `tug status` adds the env
  label, production and profile.

### Diagnostics

- **`tug check`** runs policy packs against workloads and other objects:
  required labels, image registries, requests and limits, probes,
  PodDisruptionBudgets, securityContext, topology spread, storage classes and
  custom resource conditions. A built-in `baseline` pack is advisory; your
  own packs come from files or sha256-pinned URLs in `policy.packs`. It exits
  1 only when a blocking check fails.
  [Guide](https://paingha.github.io/tugcli/docs/guides/policy).
- **`tug reconcile status|explain|trace|events`** finds the controller meant
  to fix an object, checks that it runs and holds its leader Lease, and builds
  a timeline of what happened to the object, marking each finding as observed
  or inferred. [Guide](https://paingha.github.io/tugcli/docs/guides/controllers).
- **`tug watch <target>`** follows a rollout or any object's reconciliation
  live: conditions, generations, replicas, pods, restarts and, with
  `--events`, warnings. `--until ready|deleted` with `--timeout` makes it a
  wait for scripts.
- **`tug support bundle <target>`** collects an object or namespace, its
  diagnosis, events, logs, nodes, the API server's version and `tug doctor`'s
  checks into one `.tar.gz` for a ticket. Redaction is always on: no
  Secret's data, no env or ConfigMap values, and credentials scrubbed from
  logs and events. `--preview` lists the files and writes nothing.
- **`tug diff`** compares live objects with manifests through a server-side
  dry run, or with the same object on another cluster (`--against`), and says
  which field manager owns each field that differs. **`tug drift`** compares
  two clusters' API versions, CRDs and objects. Both exit 0, 1 or 2 for CI.
  [Guide](https://paingha.github.io/tugcli/docs/guides/diff).
- **`tug api resources|versions|deprecations|server-version|diff`** and
  **`tug crd explain|schema`** explore what a cluster serves, which
  deprecated APIs it still uses and how many objects were last written at
  them, and how long its Kubernetes version is supported.
  [Guide](https://paingha.github.io/tugcli/docs/guides/apis).
- **`tug gitops status|explain|drift|dependencies`** connects what Flux and
  Argo CD report to the objects they manage, and says where delivery fails:
  suspended, a dependency, the source, the build or apply, or the workloads.
  It only reads. [Guide](https://paingha.github.io/tugcli/docs/guides/gitops).
- `check`, `reconcile`, `watch`, `api`, `crd` and `gitops` run across clusters
  with `--cluster-selector`.

### Shortcuts

- **Aliases** (`tug alias set|ls|rm`): your own shortcuts for tug and kubectl
  commands. An expansion that may write goes through the read-only guard like
  any other command.
- **Bookmarks** (`tug bookmark add|ls|rm|open`) save an exact object, or a
  cluster and namespace, and open it with `why`, `tree`, `logs`, `describe`
  or `pf`, on another cluster without leaving yours.
- **`tug pick`** is a full-screen, filterable list of objects that runs an
  action on the one you choose. Without a terminal, `--query` and `--action`
  do the same.
- **`tug open <target>`** finds the file in your repository that defines a
  live object, through kustomize, Flux, Argo CD, Helm or a search, and opens
  it in your editor at the line.
  [Reference](https://paingha.github.io/tugcli/docs/reference/commands/shortcuts).

### AI explanations

- **`tug why <target> --deep`** asks a model you choose (Anthropic, OpenAI,
  Mistral, or any OpenAI-compatible API) to explain a diagnosis. Evidence is
  redacted, shown word for word with the estimated cost before anything is
  sent, and the answer's facts must cite it. tug runs nothing the answer
  suggests, and lists changes as needing your approval.
- **`tug intelligence status|usage|key|models`** shows the provider, this
  month's spend and the price table, and keeps API keys in the credential
  store. [Guide](https://paingha.github.io/tugcli/docs/guides/ai-explanations).

### tugd, credentials and the audit log

- When a cluster refuses tug's credential, tugd says so and fetches a new one
  for the next request, instead of using the refused one until it expires.
  Provider errors and their hints reach kubectl.
- Requests no longer hang on a dead connection after the machine sleeps or
  changes network.
- After tugd restarts, a refused session token says what happened and that
  running the command again fixes it.
- `tug audit` records sessions, unlocks, refused writes and outcomes, and
  gains `--session`, `-n` and `--event`.
- A [page on what the proxy protects and what it doesn't](https://paingha.github.io/tugcli/docs/concepts/proxy-limits).

### Plugins

- **Version pins:** `plugins.pins`, or `tug plugin install name@version --pin`.
- **Revocation and key rotation:** an index can revoke archives and hand its
  signing key over to a new one. Publishers use `tug plugin index revoke` and
  `tug plugin index rotate`.
- **Execution limits:** `spec.limits` in a manifest (CPU seconds, memory and
  per-operation timeouts), capped by `plugins.limits`. Linux enforces CPU and
  memory, macOS CPU, and Windows timeouts; memory also caps WebAssembly
  builds.
- **`tug plugin inspect`** explains what a plugin may access, without
  installing it: secrets, environment, network hosts, files, programs,
  sandbox, limits, signer, revocation and pin.

### Fixes

- A plugin version such as `../../x` could install outside the plugins
  directory.
- A plugin could put escape sequences on your terminal through its messages.
- A plugin that answered a serve request twice could hang tug, and a very
  large `retryAfterSeconds` turned into a negative wait.
- SDK plugins in serve mode could interleave their error output.
- A WebAssembly plugin took seconds to stop when interrupted while starting.
- The file credential store (`TUG_KEYRING_BACKEND=file`) failed now and then
  on Windows when used from several processes at once.
- `tug import --help` showed a setting the CoreWeave plugin doesn't have, an
  ambiguous match printed a doubled colon, and counts of one read "1
  clusters".

### Project

- A [security guide](https://paingha.github.io/tugcli/docs/concepts/security),
  [SECURITY.md](SECURITY.md) for reporting vulnerabilities privately, a
  [code of conduct](CODE_OF_CONDUCT.md), and issue forms and a pull request
  template.

## 0.1.0 to 0.1.4

The first releases: the catalog and providers, per-terminal sessions,
credentials in the OS keychain, the plugin protocol and SDKs, tugd, OIDC
sign-in, directory pins and the TUI.
