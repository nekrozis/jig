# jig

`jig` installs and maintains [wshim](https://github.com/nekrozis/wshim) shims on Windows.

A wshim shim is an executable that resolves `<its own directory>\.ref\<its own file name>`
as a symbolic link and launches the link target with the same arguments, environment,
standard handles and working directory. At runtime it is two statements: `read_link`, then
`spawn`. Everything that can go wrong therefore has to be prevented at install time,
because a contract violation at runtime is a process abort rather than a readable error.

`jig` is that install-time half. It owns the `.ref` entries, the shim copies, and a
manifest that lets `check` prove the tree is still coherent.

```
jig.exe add git "C:\Program Files\Git\cmd\git.exe"
git --version          :: now goes through <root>\bin\git.exe
```

## The binaries are upstream, unmodified

`jig` embeds the official `wshim-c.exe` and `wshim-w.exe` from
[`nekrozis/wshim` **v0.2.0**](https://github.com/nekrozis/wshim/releases/tag/v0.2.0)
verbatim. It does not build them.

The reason is provenance, not convenience. A locally compiled Rust/MSVC binary is a byte
stream nobody else can vouch for: the toolchain, the image and the linker decide the hash,
so a self-built shim can never be checked against anything. The upstream artifacts come
with a SLSA provenance statement that binds these exact digests to a tag, a commit and a
GitHub-hosted workflow run, so anyone can verify where the code came from.

| file | size | sha256 |
| --- | --- | --- |
| `wshim-c.exe` | 310272 | `65027fea0dd526e98301184e070468adf9352d7cd99e0ff0af572a640fe488e3` |
| `wshim-w.exe` | 310272 | `b3cdb49fbbeba420de0b85a4d121623661ec5b285ef1825d5741950501cf8deb` |

The digests are pinned in `asset.go` and re-checked on every start, so a build that picked
up the wrong file fails immediately instead of producing shims that forward somewhere
unexpected.

### Verifying that your `jig` carries the real thing

```console
$ jig version
jig 0.3.0 (go1.27.3)
templates from nekrozis/wshim v0.2.0 (9635fe2b46b5)
  wshim-c.exe  sha256=65027fea...fe488e3  size=310272  subsystem=3
  wshim-w.exe  sha256=b3cdb49f...c1cf8deb  size=310272  subsystem=2
```

Compare those two digests with `sha256.txt` on the upstream release. That is the only
self-identification a user can perform: *which two shims did I actually get?*

To check the release itself:

```console
$ gh attestation verify wshim-c.exe --repo nekrozis/wshim \
    --signer-workflow nekrozis/wshim/.github/workflows/release-build.yml \
    --source-digest 9635fe2b46b5277aae17a7143cb97d7f64d39a07 \
    --source-ref refs/tags/v0.2.0
```

## Install

```console
$ jig init --root D:\shims
$ setx PATH "%PATH%;D:\shims\bin"
```

`init` writes the skeleton and both templates, and plants a `.jig-root` marker. `jig.exe`
can live anywhere, including inside `<root>\bin`.

## Where the root comes from

`jig` never assumes its own directory is the root. Package managers install tools wherever
they like - `scoop\apps\jig\current`, a per-user tools folder, a winget link farm - and
adopting that directory as the shim root would scatter `bin\`, `.ref\` and `shim.db` into a
tree `jig` does not own.

Resolution order:

1. `--root <dir>`
2. `$JIG_ROOT`
3. the nearest ancestor of `jig.exe` carrying a `.jig-root` marker

Rule 3 keeps "`jig` lives in `<root>\bin`" convenient without turning it into an
assumption. Installed anywhere else, `jig` finds no marker and tells you to pass `--root`.

## On-disk layout

```
<root>\
  .jig-root           marker: "this directory is a jig root"
  shim.db             append-only event log; gc compacts it to one snapshot line
  .lock               held exclusively during mutations
  templates\
    wshim-c.exe       console-subsystem template
    wshim-w.exe       GUI-subsystem template
  bin\                <- put this on PATH
    jig.exe           optional; jig can live here
    <name>.exe        a copy of one template, bytes identical
    .ref\
      <name>.exe      absolute symlink -> the real program
```

`.ref` must be a sibling child of the shims, not a sibling of `bin`. A running shim
resolves `<its own directory>\.ref\<its own file name>`, so moving `.ref` anywhere else
breaks every shim at runtime, silently.

A shim is a *copy*, never a link. Launching it therefore yields `GetModuleFileNameW` =
`<root>\bin\<name>.exe`, and the shim reads `.ref\<name>.exe` next to itself. Hard links,
symlinks and junctions all work too, but a copy is the only form that keeps the two names
independent of each other.

## Commands

```
jig [--root <dir>] <subcommand> [args...]

add     <name> <target> [--as <alias>]... [--force] [--allow-script]
rm      <name>... [--force] [--keep-target]
ls      [--json]
check   [<name>...] [--fix] [--json]
which   <name> [--json]
gc      [--dry-run|--apply]
path
init    [--force]
version [--json]
```

Exit codes: `0` ok, `1` partial failure, `2` usage, `3` invalid state, `4` missing
privilege, `5` lock contention.

### `add` order is not negotiable

1. validate the name (reserved device names, trailing dot/space, `.exe` suffix, path
   separators)
2. resolve the target and inspect it: exists -> regular file -> `.exe` -> PE -> machine ->
   subsystem
3. reject cycles: a target inside `bin\` or `.ref\` means `a -> b -> a`
4. reject `templates\` as a target (it would replay the same forward, and `templates\.ref`
   does not exist)
5. write the template to `templates\`
6. create `.ref\<name>.exe.tmp.<rand>`, then rename it onto `.ref\<name>.exe`
7. write the shim copy to `bin\<name>.exe`
8. append the manifest record

Steps 6 and 7 are in that order because visibility must mean usability: the instant
`bin\<name>.exe` exists, the user can invoke it.

### What `check` can repair

| code | meaning | `--fix` |
| --- | --- | --- |
| `missing` | named on the command line, exists nowhere | no |
| `stale` | manifest record, neither file on disk | drops the record |
| `orphan` | shim copy with no `.ref` entry | removes the copy |
| `half` | `.ref` entry with no shim copy | removes the entry |
| `not-symlink` | `.ref` entry is not a symlink | no |
| `dangling` | link target is gone | no |
| `cycle` | target resolves inside the shim directory | no |
| `relative` | relative link, depends on the shape of `.ref` | rewrites absolute |
| `not-pe` | target is not a PE image | no |
| `template` | shim copy differs from its template | rewrites the copy |
| `foreign` | executable in `bin` that `jig` never wrote | **never touches it** |
| `unregistered` | executable in `bin` not in the manifest | never deletes |

`orphan` only fires when the digest matches a template. An executable `jig` did not write
belongs to whoever put it there, and deleting it would be data loss - which matters because
`bin` may be a directory shared with another package manager. `jig.exe` itself is excluded
from the inventory so that `jig check --fix` can never remove the manager.

`gc` defaults to a dry run and only handles three things: leftover `.tmp.` files, stale
manifest records, and compaction when the log exceeds 2000 events or 1 MiB.

## Why `go:embed`

One `jig.exe` is the entire distribution. No sidecar files, no "the binary and its
templates got separated". The cost is a rebuild when the upstream tag moves, which is the
point: the pinned digests are a compile-time contract, not a runtime guess.

## Build

```console
$ go build -trimpath -ldflags "-s -w" -o jig.exe .
```

No external modules. `go 1.21` or newer.

## Non-goals

- No fallback when symlink creation is denied. Developer Mode (or one elevated run) is
  required; a shim only understands `read_link`, so there is nothing to fall back to.
- No argument rewriting, no environment filtering, no output capture. The shim forwards
  handles and arguments; `jig` never sits in the execution path.
- No PATH editing. `jig path` prints the directory; changing the environment is the
  caller's business.
