# jig

`jig` installs and maintains [wshim](https://github.com/nekrozis/wshim) shims on Windows.

A wshim shim is an executable that resolves `<its own directory>\_ref\<its own file name>`
as a symbolic link and launches the link target with the same arguments, environment,
standard handles and working directory. At runtime it is two statements: `read_link`, then
`spawn`. Everything that can go wrong therefore has to be prevented at install time,
because a contract violation at runtime is a process abort rather than a readable error.

`jig` is that install-time half. It owns the `_ref` entries, the shims, and a
manifest that lets `check` prove the tree is still coherent.

```
jig.exe add "C:\Program Files\Git\cmd\git.exe"   :: shim name is git
jig.exe add git "C:\Program Files\Git\cmd\git.exe"
git --version          :: now goes through <root>\bin\git.exe
```

## The binaries are upstream, unmodified

`jig` embeds the official `wshim-c.exe` and `wshim-w.exe` from
[`nekrozis/wshim` **v0.3.0**](https://github.com/nekrozis/wshim/releases/tag/v0.3.0)
verbatim. It does not build them.

The reason is provenance, not convenience. A locally compiled Rust/MSVC binary is a byte
stream nobody else can vouch for: the toolchain, the image and the linker decide the hash,
so a self-built shim can never be checked against anything. The upstream artifacts come
with a SLSA provenance statement that binds these exact digests to a tag, a commit and a
GitHub-hosted workflow run, so anyone can verify where the code came from.

| file | size | sha256 |
| --- | --- | --- |
| `wshim-c.exe` | 310784 | `4b2cb490278bd226a92a2c40eb705464edc3c08ab536a969f24c3cb1899f7ce7` |
| `wshim-w.exe` | 310784 | `a2943cc4a280512fbdc605ae69cdce5cac10c657d9d056cde522bb19751183c3` |

The digests are pinned in `asset.go` and re-checked on every start, so a build that picked
up the wrong file fails immediately instead of producing shims that forward somewhere
unexpected. `assets/upstream-manifest.json` is the release's own build record (image,
toolchain, linker, flags), kept verbatim so the provenance can be audited offline.

### Verifying that your `jig` carries the real thing

```console
$ jig version
jig 0.1.0 (go1.27.3)
templates from nekrozis/wshim v0.3.0 (2a652f9222d9)
  wshim-c.exe  sha256=4b2cb490...9f7ce7  size=310784  subsystem=3
  wshim-w.exe  sha256=a2943cc4...51183c3  size=310784  subsystem=2
```

Compare those two digests with `sha256.txt` on the upstream release. That is the only
self-identification a user can perform: *which two shims did I actually get?*

To check the release itself:

```console
$ gh attestation verify wshim-c.exe --repo nekrozis/wshim \
    --signer-workflow nekrozis/wshim/.github/workflows/release-build.yml \
    --source-digest 2a652f9222d9b5d3978a57c5025dd052bf30e98a \
    --source-ref refs/tags/v0.3.0
```

## Install

```console
$ jig init --root D:\shims
```

Then add `D:\shims\bin` to PATH through **Settings > System > About > Advanced system
settings > Environment Variables**, or the "Edit environment variables for your account"
dialog. Avoid `setx PATH "%PATH%;..."`: it expands the system and user values into the
user value, duplicates entries, and truncates at 1024 characters.

`init` writes the skeleton and both templates, and plants a `.jig-root` marker. `jig.exe`
can live anywhere, including inside `<root>\bin`.

## Where the root comes from

`jig` never assumes its own directory is the root. Package managers install tools wherever
they like - `scoop\apps\jig\current`, a per-user tools folder, a winget link farm - and
adopting that directory as the shim root would scatter `bin\`, `_ref\` and `shim.db` into a
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
    <name>.exe        hard link to one template (copy where linking is impossible)
    _ref\
      <name>.exe      absolute symlink -> the real program
```

`_ref` must be a sibling child of the shims, not a sibling of `bin`. A running shim
resolves `<its own directory>\_ref\<its own file name>`, so moving `_ref` anywhere else
breaks every shim at runtime, silently.

A shim is a real file of its own, never a symlink to the target: launching it yields
`GetModuleFileNameW` = `<root>\bin\<name>.exe`, and the shim reads `_ref\<name>.exe` next to
itself. Taking a symlink or a junction to the target instead works too, but only a real file
keeps the two names independent of each other.

What the shim *is* linked to is the template. `bin\<name>.exe` is a hard link into
`templates\`, so every shim of one subsystem shares a single 310 KB inode: 100 console shims
cost 310 KB, not 31 MB, and a running set of them maps one image section. `os.Link` is
attempted first and any failure falls back to a copy, so exFAT volumes and a `bin\` junctioned
onto another drive still work - they just cost the disk space. NTFS's ceiling of 1024 links
per file is not left as a cliff either: the template is re-materialised on a fresh inode and
the link retried once, so the 1024th shim costs one extra 310 KB template rather than
turning every later shim into a full copy.

The price of sharing an inode is one rule: **never write to `templates\` in place.** jig
always replaces it through a temp file and a rename, which detaches the old inode rather
than mutating it, so an upgrade cannot rewrite the bytes its shims are pointing at. An
outside program that truncates a template in place breaks every linked shim at once;
`jig check` names them all and `--fix` restores them from the embedded copy.

Windows cannot replace an image that is in use. Close running shims before upgrading;
the refresh fails cleanly otherwise, and the same command can be retried.

## Commands

```
jig [--root <dir>] <subcommand> [args...]

add     [<name>] <target> [--as <alias>]... [--force] [--allow-script]
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

1. validate the name (explicit, or the target's basename without `.exe`/`.bat`/`.cmd`;
   reserved device names, trailing dot/space, path separators are rejected)
2. normalize the input and resolve it for inspection: exists -> regular file -> `.exe` ->
   PE -> machine -> subsystem
3. reject cycles on the resolved path: inside `bin\` or `_ref\` means `a -> b -> a`
4. reject a resolved target inside `templates\` (it would replay the same forward, and
   `templates\_ref` does not exist)
5. create `root`, `bin\` and `_ref\`
6. take the lock
7. write the root marker
8. write the template to `templates\`
9. refuse to replace a `bin\<name>.exe` that jig did not write, unless `--force`
10. check the stored target for idempotence
11. create `_ref\<name>.exe.tmp.<rand>`, then rename it onto `_ref\<name>.exe`
12. link the template into `bin\<name>.exe` (copy if linking is unavailable)
13. append the manifest record

Steps 11 and 12 are in that order because visibility must mean usability: the instant
`bin\<name>.exe` exists, the user can invoke it.

Step 9 is the third face of one rule, because `bin\` may be shared with another package
manager: a file whose digest matches no template is not jig's. `check` reports it as
`foreign` and never deletes it, `rm` refuses to delete it, and `add` refuses to replace it.
The refusal happens before `_ref` is written, so it leaves the tree exactly as it was.

`add` stores the normalized path you gave, not the fully resolved one. A Scoop-style
`current` junction therefore keeps tracking upgrades; existence, PE inspection and cycle
checks use the resolved path. Pass the versioned path when you want to pin a version.

### What `check` can repair

`check` reads without the lock. `check --fix` takes it first, then reloads the manifest, so
it cannot race an `add` that is halfway through creating a temporary file.

| code | meaning | `--fix` |
| --- | --- | --- |
| `missing` | named on the command line, exists nowhere | no |
| `stale` | manifest record, neither file on disk | drops the record |
| `orphan` | manifest record, but `_ref` has no entry | removes the shim |
| `half` | `_ref` entry with no shim | removes the entry |
| `not-symlink` | `_ref` entry is not a symlink | no |
| `dangling` | link target is gone | no |
| `cycle` | target resolves inside `bin\`, `_ref\`, or `templates\` | no |
| `relative` | relative link, depends on the shape of `_ref` | rewrites absolute |
| `not-pe` | target is not a PE image | no |
| `template` | shim differs from its template | relinks from the template |
| `foreign` | executable in `bin` that `jig` never wrote | **never touches it** |
| `unregistered-shim` | bytes match a template, but no manifest record | never deletes |
| `unregistered` | executable in `bin` not in the manifest | never deletes |

A digest match proves the file contains wshim, not that `jig` created it. The manifest
record is the ownership proof: `orphan` removes a shim only when a record says `jig` put it
there. Matching bytes without a record are reported as `unregistered-shim` and kept.
An executable `jig` did not write belongs to whoever put it there, and deleting it would be
data loss - which matters because `bin` may be shared with another package manager.
`jig.exe` itself is excluded from the inventory so that `jig check --fix` can never remove
the manager.

After a template upgrade, an orphan written by the previous version matches no current
template and is reported as `foreign`, so it is kept. Once the bytes change, ownership
cannot be proven, and deleting it would risk someone else's file.

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

No external modules. `go 1.27.3` or newer.

## Non-goals

- No fallback when symlink creation is denied. Developer Mode (or one elevated run) is
  required; a shim only understands `read_link`, so there is nothing to fall back to.
- No argument rewriting, no environment filtering, no output capture. The shim forwards
  handles and arguments; `jig` never sits in the execution path.
- No PATH editing. `jig path` prints the directory; changing the environment is the
  caller's business.
- No arm64 targets: the embedded shim is x64, so such a shim would fail at launch.

## License

BSD 3-Clause. See [LICENSE](LICENSE).
