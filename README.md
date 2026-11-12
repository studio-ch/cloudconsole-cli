# cloudconsole

Command-line interface for the Cloud Console. A single static binary for
managing Xcloud instances, snapshots, data volumes, images, networks, security
groups and elastic IPs — from a terminal or from CI.

> This repository is a **read-only mirror**, published from the Cloud
> Console monorepo on every release. Issues and feature requests are
> welcome here; pull requests are not, since changes here are overwritten
> by the next mirror — see [CONTRIBUTING.md](./CONTRIBUTING.md).

## Install

**Homebrew**

```bash
brew install studio-ch/tap/cloudconsole
```

**Shell installer** (Linux and macOS, verifies the checksum)

```bash
curl -fsSL https://raw.githubusercontent.com/studio-ch/cloudconsole-cli/main/install.sh | sh
```

Pin a version, or choose where it lands:

```bash
CLOUDCONSOLE_VERSION=v0.1.0 CLOUDCONSOLE_INSTALL_DIR="$HOME/.local/bin" \
  sh -c "$(curl -fsSL https://raw.githubusercontent.com/studio-ch/cloudconsole-cli/main/install.sh)"
```

**Go**

```bash
go install github.com/studio-ch/cloudconsole-cli/cmd/cloudconsole@latest
```

**Manual** — download an archive from
[Releases](https://github.com/studio-ch/cloudconsole-cli/releases), verify it
against `SHA256SUMS`, and put `cloudconsole` on your `PATH`. Windows builds are
published as `.zip`.

```bash
cloudconsole version
```

## Authenticate

Issue an API key in the panel under **Settings → API keys**. The secret is
shown once, at creation. Choose the **Read + Write** preset if you intend
to change anything — a read-only key is rejected on mutations.

```bash
cloudconsole auth login     # prompts, without echoing
cloudconsole auth status    # organisation, key, scopes, expiry
```

In CI, set the token in the environment instead:

```bash
export CLOUDCONSOLE_API_TOKEN=sk_live_…
```

A key belongs to exactly one organisation. For several, use one profile
per key (`cloudconsole auth login --profile acme`, then `--profile acme` or
`cloudconsole config use acme`).

## Use

```bash
cloudconsole region list
cloudconsole instance list
cloudconsole instance list -o json | jq -r '.[].name'

cloudconsole instance create \
  --name build-01 --region ZRH1 \
  --image ghcr.io/example/macos-sequoia:latest \
  --cpu 10 --memory 28 --disk 480 --wait

cloudconsole instance suspend <id> --wait      # suspend to disk
cloudconsole instance boot-mode <id> --recovery --wait
cloudconsole instance stop <id> --wait         # required before deletion
cloudconsole instance delete <id> --yes --wait

cloudconsole volume create --name scratch --region ZRH1 --size 500
cloudconsole volume attach <volume-id> --instance <instance-id>

cloudconsole snapshot create --instance <id> --name before-upgrade --wait
cloudconsole snapshot list --instance <id>
cloudconsole instance restore <id> --snapshot before-upgrade --yes --wait
cloudconsole instance create --name build-02 --from-snapshot before-upgrade --wait
cloudconsole snapshot delete before-upgrade --yes --wait
cloudconsole instance delete <id> --delete-snapshots --yes   # snapshots outlive the instance otherwise
```

Most instance operations are asynchronous. `--wait` blocks until the
instance reaches its new state — use it whenever you script two operations
in a row, because a second action while one is pending is rejected.

`cloudconsole --help` lists everything; `cloudconsole <command> --help` goes deeper.

## Linux startup configuration

Pass a complete first-boot document with `--user-data-file` and
`--user-data-format`. For Fedora CoreOS, use Ignition JSON (convert Butane YAML
first):

```bash
cloudconsole instance create \
  --name coreos-01 --region ALP2 --platform linux \
  --image coreos --cpu 2 --memory 4 --disk 40 \
  --admin-username core \
  --user-data-file ./config.ign --user-data-format ignition --wait
```

Choose an image and sizing available in your region. For cloud-init images, use
`--user-data-file ./cloud-config.yaml --user-data-format cloud-init`; the YAML
must start with `#cloud-config`. Use `--user-data-file -` to read stdin.

The document replaces automatic setup. Define users and SSH keys inside it and
omit `--ssh-key`. `--admin-username` records the user for connection information;
it does not create a user when you provide custom data. Documents are limited to
64 KiB of UTF-8, encrypted by the API, and never returned by instance reads. They
run only on first boot; changing a local file does not reconfigure an existing VM.

## Computer use

`cloudconsole instance display` lets a script or an AI agent see and operate
the screen of a running instance, for applications that have no command line:

```sh
cloudconsole instance display screenshot <id> --file screen.png
cloudconsole instance display click <id> --x 640 --y 400
cloudconsole instance display type <id> --text "hello world"
cloudconsole instance display press <id> --key cmd+space
cloudconsole instance display drag <id> --x 200 --y 300 --to-x 600 --to-y 300
```

Coordinates are pixels of the screenshot, from its upper-left corner. A
screenshot is as large as the screen, which on a HiDPI Mac is more than a
vision model needs. `--scale 0.5` halves it; pass the same `--scale` to the
pointer commands so a point read from the picture lands where you saw it.

An agent works in a loop: take a screenshot, let the model pick the next
action, send it, and take another screenshot to see the result. Input commands
return as soon as the input is sent, before the instance has reacted.

Screen and input come from the instance's virtual display, so this works for
macOS and Linux, at the login window too, and needs nothing installed in the
instance. Text is typed on a US keyboard layout: ASCII everywhere, and on macOS
also accented letters, the euro sign and typographic punctuation. The display
has no mouse wheel and no Page Up or Page Down; scroll with space, the arrow
keys, or `cmd+up` and `cmd+down` on macOS. The API key needs the
`write:resources` scope.

## Scripting

`--output json` emits the API's response body **unchanged**, with only the
list envelope unwrapped. Field names and types are exactly what the API
returns, so `jq` recipes transfer between `curl` and `cloudconsole`. Table output
carries no such promise — do not parse it.

Progress and warnings go to stderr, so this is safe:

```bash
id=$(cloudconsole instance create … --wait -o json | jq -r .id)
```

Exit codes are stable; `cloudconsole exit-codes` prints the full table. The ones
worth branching on:

| Code | Meaning |
|---:|---|
| 0 | success |
| 3 | authentication failed |
| 4 | permission denied (read-only key, or a disabled service) |
| 5 | not found |
| 6 | the resource is busy with another operation |
| 7 | quota exceeded |
| 12 | `--wait` timed out (the operation may still finish) |

### GitHub Actions

```yaml
- name: Provision a build VM
  env:
    CLOUDCONSOLE_API_TOKEN: ${{ secrets.CLOUDCONSOLE_API_TOKEN }}
  run: |
    curl -fsSL https://raw.githubusercontent.com/studio-ch/cloudconsole-cli/main/install.sh | sh
    id=$(cloudconsole instance create \
      --name "ci-${GITHUB_RUN_ID}" --region ZRH1 \
      --image ghcr.io/example/macos-sequoia:latest \
      --cpu 10 --memory 28 --disk 480 \
      --wait -o json | jq -r .id)
    echo "INSTANCE_ID=$id" >> "$GITHUB_ENV"

- name: Tear down
  if: always()
  env:
    CLOUDCONSOLE_API_TOKEN: ${{ secrets.CLOUDCONSOLE_API_TOKEN }}
  run: |
    cloudconsole instance stop "$INSTANCE_ID" --wait
    cloudconsole instance delete "$INSTANCE_ID" --yes --wait
```

`--yes` is required for destructive commands without a terminal — the CLI
refuses rather than guessing.

## Configuration

`~/.config/cloudconsole/config.yaml`, created with `0600` permissions because it
holds a token; the CLI refuses to read it if the permissions are looser.

Settings resolve **per field**: flag, then environment
(`CLOUDCONSOLE_API_URL`, `CLOUDCONSOLE_API_TOKEN`, `CLOUDCONSOLE_OUTPUT`, `CLOUDCONSOLE_PROFILE`),
then the profile, then the default. So a token from the environment
combines with a URL from the profile — the usual CI arrangement.
`cloudconsole config explain` shows what was resolved and from where.

The default endpoint is `https://api.cloud.flow.swiss`. Point the CLI at a
different deployment with `--api-url`, `CLOUDCONSOLE_API_URL`, or an
`api_url` in the profile — one profile per environment is the usual shape.

To keep the secret out of the file, use a command instead:

```yaml
profiles:
  prod:
    api_url: https://api.cloud.flow.swiss
    token_command: op read op://Private/cloud/api-key
```

## Troubleshooting

`--debug` traces every request to stderr with the credential redacted to
its public prefix. Every error carries a `request-id` — quote it to
support.

## Related

- **REST API** — the CLI is a client of the documented public API; anything
  it does can be done with `curl`.
- **MCP server** — connect an AI client to your account with the same key.
- **Packer plugin** —
  [studio-ch/packer-plugin-cloudconsole](https://github.com/studio-ch/packer-plugin-cloudconsole)
  bakes custom macOS images.

## Not covered

The CLI authenticates with an API key, and some parts of the panel are not
reachable that way: the VM console and terminal, the AI assistant, the
admin area, and issuing or revoking API keys (listing works; issuing is a
panel operation).

### Private networks

Xcloud networks are isolated private IPv4 networks with NAT egress. There is no
`--mode` switch. Both `--cidr` (RFC 1918 subnet) and `--gateway` are required.
Provide both DHCP endpoints to enable address assignment; omit both to disable it.

```sh
cloudconsole network create --name private --region ALP2 \
  --cidr 10.42.0.0/24 --gateway 10.42.0.1 \
  --dhcp-range-start 10.42.0.100 --dhcp-range-end 10.42.0.200
```

The API retains `cidr` for compatibility and maps it to upstream `spec.subnet`.
DHCP is now an object with `rangeStart` and `rangeEnd`, rather than a boolean.
Old `--mode` and `--dhcp` flags must be replaced as shown above.

Update an existing tenant-owned network without replacing it:

```sh
cloudconsole network update private --region ALP2 --gateway 10.42.0.2
cloudconsole network update private --region ALP2 --disable-dhcp
```

Only explicitly supplied fields change. Global catalog networks and their seeded
copies are read-only. Address changes can interrupt connected instances.
