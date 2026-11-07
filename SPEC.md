# jig - implementation specification

> Companion to the runtime layer `github.com/nekrozis/wshim`. This document covers the
> **install-time** tool only.
>
> Version: 2026-11-07, v5 (wshim 0.3.0 templates, _ref directory).
> Runtime: `nekrozis/wshim` **v0.3.0**, tag `v0.3.0`, commit
> `2a652f9222d9b5d3978a57c5025dd052bf30e98a` (`src/lib.rs` + `src/ctrl.rs`).

## 0. Fixed behaviour of the embedded template

These are properties of the upstream binary. They are not negotiable; they are the reason
the manager has to be strict.

| Behaviour | Upstream implementation | Consequence for `jig` |
| --- | --- | --- |
| Reading `_ref` | `fs::read_link(link).unwrap()` | the entry **must** be a symlink and must exist |
| Target legality | `assert!(target.is_absolute())` | a relative target cannot be written at all |
| Failure mode | panic hook is silent, `panic = "abort"` | exit code `0xC0000409`; no stdout or stderr output on either subsystem |
| Forwarding | `Command::new(&target).args(skip(1)).status()` | target must be `.exe`; `.bat` is re-parsed by cmd |
| Exit code | `process::exit(status.code().unwrap())` | `code()` is always `Some` on Windows |
| Ctrl+C | `SetConsoleCtrlHandler(handler, 1)` returning TRUE | the shim does not die early; the child shares the console |
| Recursion | **no protection of any kind** | cycle detection is entirely the manager's job |

A contract violation is a silent abort, not an error message. Therefore:

- the manager is the only line of defence, and every check happens before a byte is written;
- a shim that fails at runtime produces no diagnostics, so `check` must be able to prove
  the tree is coherent without running anything.

## 1. Scope

| Dimension | Decision |
| --- | --- |
| Role | the only program allowed to create, modify or delete shim structure |
| When it runs | install, upgrade, uninstall, troubleshooting. **Never on the forwarding path** |
| Language | Go 1.27.3+, standard library only (`syscall` only for `CreateFileW` locking) |
| Distribution | one `jig.exe`; `go:embed` carries both templates (3.8 MB total) |
| Root | `--root` > `$JIG_ROOT` > nearest `.jig-root` marker above `jig.exe` |
| Not done | PATH editing, registry writes, version management, running anything |

**Core claim**: concentrate all uncertainty and all validation on the write side, so the
runtime is left with `read_link` and `spawn`.

## 2. Disk contract

```
<root>\
  .jig-root                     marker, contents "jig-root 1\n"
  shim.db                       manifest: JSONL, append-only, never read at runtime
  .lock                         held exclusively during mutations
  templates\
    wshim-c.exe                 console template
    wshim-w.exe                 GUI template
  bin\                          the only directory that goes on PATH
    jig.exe                     optional
    <name>.exe                  hard link to one template (copy if linking fails)
    _ref\
      <name>.exe                absolute symlink -> the real program
```

The templates live in `assets/` in the source tree and are embedded at **compile time**.
`templates\` is the **runtime** copy; it is byte-identical to the embedded bytes and is
rewritten whenever the digest disagrees. An upgrade therefore needs no sidecar files:
`check --fix` refreshes the on-disk templates.

A shim is normally a **hard link** to its template, so N shims occupy one 310 KB image
instead of N. `os.Link` is attempted first and every failure falls back to a byte copy, so
the contract that callers depend on never varies: a shim is an executable whose digest
equals the pinned template. The fallbacks are a filesystem without hard links (exFAT,
FAT32) and a `bin\` junctioned onto another volume.

NTFS caps one file at 1024 links (measured: the 1024th returns `ERROR_TOO_MANY_LINKS`,
1142). The ceiling is rolled over, not accepted: rewriting the template at the same path
leaves a fresh inode holding the same bytes, while the shims already linked to the previous
one keep it alive, so one retry buys another 1023 slots. Nothing new appears in
`templates\`, so there is no second class of file to name, verify, invalidate on upgrade or
garbage collect.

Because the inode is shared, one rule is load-bearing: **`templates\` is never written in
place.** Every write goes through `writeAtomic` (temp file + rename), which detaches the old
inode instead of mutating it, so an upgrade can never rewrite the bytes a shim is pointing
at. Nothing stops an outside program from truncating the template in place, though, and if
it does, every linked shim breaks at once - `check` reports them all and `--fix` restores
them from the embedded copy.

Three non-negotiables:

1. A shim reads `<its own directory>\_ref\<its own file name>` and nothing else.
2. That entry must reach a final executable without forming a cycle.
3. `bin\` is the single PATH entry; no recursion, no extra directories.

## 3. Link forms

| Form | Cross-volume | Privilege | Can a shim read it | Verdict |
| --- | --- | --- | --- | --- |
| Symlink, file, absolute target | yes | Developer Mode | yes, `read_link` | **the only legal form** |
| Hard link | no, same volume only | none | no, `read_link` returns 4390 | forbidden |
| Text file | yes | none | no, upstream has no such branch | forbidden |
| Junction | n/a | none | no, directory-level only | forbidden |

- **Hard links cannot work**: `fs::read_link` reports `os error 4390`, so the shim never
  obtains a target.
- **There is no fallback.** The upstream implementation has no "read it as a text file"
  branch, and adding one would introduce runtime open/read plus BOM and whitespace
  handling, contradicting the zero-tolerance design.
- **Privilege is detected, not guessed**: `CreateSymbolicLinkW` failing with
  `1314 ERROR_PRIVILEGE_NOT_HELD` maps to exit code 4, with the two known ways out
  (enable Developer Mode, or run once elevated). No silent downgrade.

## 4. Root resolution

`jig` never treats its own directory as the root. Package managers install tools wherever
they like, and silently adopting that location would scatter `bin\`, `_ref\` and `shim.db`
into a tree `jig` does not own.

Resolution order, first match wins:

1. `--root <dir>`
2. `$JIG_ROOT`
3. the nearest ancestor of the running `jig.exe` that contains a `.jig-root` file
4. otherwise: exit code 2, with a message naming all three attempts

Rule 3 exists so that "`jig` installed at `<root>\bin\jig.exe`" keeps working without a
flag. It is a declared property of the tree, not an inference from the executable's
location: a `jig` installed at `scoop\apps\jig\current\jig.exe` finds no marker and stops.

`init` and every successful mutation create the marker, so a root that `jig` has written to
is always self-describing afterwards.

## 5. CLI

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

| Exit code | Meaning |
| --- | --- |
| 0 | success, including an idempotent no-op |
| 1 | partial failure (some entries in a batch failed) |
| 2 | usage error, including "cannot determine the root" |
| 3 | invalid state (bad target, cycle, dangling link, inconsistent tree) |
| 4 | missing privilege (cannot create the symlink) |
| 5 | lock contention |

`--json` output must always be machine-parseable: an object or an array, never a bare
string, and never mixed with warnings on stdout.

## 6. `add` sequence (order is not interchangeable)

| # | Step | On failure |
| --- | --- | --- |
| 1 | derive the name when omitted (target basename without `.exe`/`.bat`/`.cmd`), validate it plus aliases, reject case-insensitive duplicates | exit 3 |
| 2 | normalize the input with `filepath.Abs` + `Clean`; resolve it with `filepath.EvalSymlinks` for validation only | `ENOENT` -> exit 3; a path that does not exist is never accepted |
| 3 | inspect the resolved path: regular file -> `.exe` -> PE -> machine -> subsystem | exit 3 |
| 4 | cycle detection on the resolved path: inside `bin\` or `_ref\` means `a -> b -> a` | exit 3 |
| 5 | reject a resolved target inside `templates\` (replays the same forward; `templates\_ref` does not exist) | exit 3 |
| 6 | create `root`, `bin\` and `_ref\` | exit 1 |
| 7 | take the lock | exit 5 |
| 8 | write the root marker | exit 1 |
| 9 | write the chosen template to `templates\` if the digest differs | exit 1 |
| 10 | per name: refuse to replace a `bin\<name>.exe` whose digest matches no template (`--force` overrides) | exit 3 |
| 11 | per name: check the stored link target for idempotence (`--force` replaces a different one) | exit 3 |
| 12 | per name: create `_ref\<name>.exe.tmp.<rand>`, then `os.Rename` onto `_ref\<name>.exe` | exit 1 |
| 13 | per name: link the template into `bin\<name>.exe` (copy when linking fails) | exit 1 |
| 14 | per name: append the manifest record | exit 1 |

Steps 11-13 run in that order for one reason: visibility must mean usability. The instant
`bin\<name>.exe` exists the user can invoke it, so every action that can fail happens
first. Removal reverses this (section 9).

Step 10 exists because `bin\` may be shared with another package manager, and the same rule
governs all three commands: a file whose digest matches no template is not jig's. `check`
reports it as `foreign` and never deletes it, `rm` refuses to delete it, and `add` refuses
to replace it. The check runs before `_ref` is touched, so a refusal cannot leave a
half-written entry behind - the tree is byte-identical afterwards.

Every rename uses `os.Rename`, which is `MoveFileExW(MOVEFILE_REPLACE_EXISTING)` on
Windows - atomic, no FFI. `writeAtomic` is `os.CreateTemp` + `Write` + `Chmod` + `Rename`,
and the temporary name always contains `.tmp.` so `check` and `gc` can recognise leftovers.

## 7. Validation rules

### 7.1 Names

When the name is omitted, it is the target's basename with `.exe`, `.bat` or `.cmd`
removed; the rules below then apply to the result.

| Rule | Reason |
| --- | --- |
| 1..127 characters | filesystem limit headroom |
| no `.exe` suffix | it is appended automatically |
| no separators or illegal characters (`\ / : * ? " < > \|`) | path injection and Win32 rejection |
| no control characters | unprintable in diagnostics |
| must not end with a space or a dot | Win32 trims them silently, so the real name differs |
| not `CON PRN AUX NUL COM1-9 LPT1-9` | reserved device names |
| unique case-insensitively | Windows cannot hold `Cargo` and `cargo` apart |

### 7.2 Targets

`resolveFinal` must succeed, the result must be a regular file, and the extension must be
`.exe` (or `.bat`/`.cmd` with `--allow-script`). A name without `.exe` is rejected because
`resolve_exe` would try `<name>.exe` first and could quietly launch a neighbour.

For `.exe`, the PE header must parse, the machine must be x86 or x64, and the
subsystem must be 2 (GUI) or 3 (console). arm64 is rejected: the embedded shim is
x64, so such a target would fail to start.

### 7.3 Template selection

Subsystem 3 -> `wshim-c.exe`; subsystem 2 -> `wshim-w.exe`. A batch file is treated as
console: `std` wraps it in `cmd.exe`, so it needs a console anyway.

### 7.4 Cycle detection

A target that resolves inside `bin\`, `_ref\`, or `templates\` is refused. The upstream runtime has no
recursion guard, so a chain `a -> b -> a` would spawn processes until an external limit
kills them, roughly 50 ms per hop with a command line that grows at every level.

### 7.5 Path length

`CreateSymbolicLinkW` reports `ERROR_FILENAME_EXCED_RANGE` (206) past `MAX_PATH`. The
retry uses an explicit `\\?\` prefix on both sides. If that also fails, the error is
reported rather than silently truncated.

## 8. `check` rules

| Code | Condition | `--fix` |
| --- | --- | --- |
| `missing` | named on the command line, exists nowhere | no |
| `stale` | manifest record, neither file on disk | drop the record |
| `orphan` | **manifest record exists**, `_ref` has no entry | remove the shim |
| `unregistered-shim` | digest matches a template, **no manifest record** | never deletes |
| `foreign` | executable in `bin` whose digest matches no template | **never touched** |
| `half` | `_ref` entry, no shim | remove the entry |
| `not-symlink` | `_ref` entry is not a symlink | no |
| `dangling` | link target is gone | no |
| `cycle` | target resolves inside `bin\`, `_ref\`, or `templates\` | no |
| `relative` | relative link; depends on the shape of `_ref` | rewrite as absolute |
| `not-pe` | target is not a PE image | no |
| `template` | shim differs from its template | relink from the template |
| `tmp` | leftover `.tmp.` file | delete it |
| `template-stale` | `templates\<t>.exe` differs from the embedded bytes | refresh it |
| `unregistered` | executable in `bin` not in the manifest | never deletes |

`orphan` requires both a manifest record and a template digest match. A digest match
alone proves the file contains wshim, not that jig created it: another manager may have
placed the same official binary in a shared `bin`. Without a record it is reported as
`unregistered-shim` and kept. A digest that matches nothing belongs to whoever put the
file there, and deleting it would be data loss.

After a template upgrade, an orphan from the previous version matches no current template
and is classified as `foreign`; it is kept, because the bytes no longer prove ownership.

`jig.exe` itself is excluded from the inventory by name, so `jig check --fix` can never
remove the running manager. Adding a shim under the running executable's own path is
refused for the same reason.

A file covered by an entry-level finding never also produces `unregistered`: one file, one
conclusion.

## 9. Removal

Reverse order: remove `bin\<name>.exe`, then unlink `_ref\<name>.exe`, then append the
record. An interruption leaves a tree that is not runnable but structurally complete, which
`check` classifies as `half`.

`rm` refuses a name that is not in the manifest. It also refuses to delete a file whose
digest no longer matches what `jig` wrote, unless `--force` is given: a mismatch means the
file is no longer `jig`'s to delete.

`os.Remove` on a symlink deletes the link only; it never follows to the target.

## 10. Concurrency, idempotence, atomicity

- `.lock` is opened with `CreateFileW` and **share mode 0**, because Go's `os.OpenFile`
  always shares read/write and that is not mutual exclusion on Windows. A 5 second
  deadline, then exit code 5.
- `add` is idempotent: the same name pointing at the same final target reports "nothing to
  do" and exits 0.
- Every file write goes through `writeAtomic`; every link goes through a temporary name
  plus rename.
- Windows refuses to replace an image that is running, so a template refresh or relink
  fails cleanly while any shim is mapped; rerunning after those processes exit completes it.
- The lock covers mutation: `add`, `rm`, `gc --apply`, `init` and `check --fix`.
  `ls`, `which` and `check` without `--fix` read without it - a stale read is preferable to
  blocking a diagnostic command behind a hung writer.

## 11. Manifest

One JSON object per line, append-only:

```json
{"v":1,"op":"add","name":"git","target":"C:\\Program Files\\Git\\cmd\\git.exe","link":"symlink","subsystem":3,"sha256":"...","ts":"2026-11-05T10:52:00+08:00"}
```

`op` is `add`, `rm` or `snapshot`. A snapshot replaces the whole state and is what `gc`
leaves behind after compaction. Entries are keyed by lower-cased name. An unparseable line
is a hard error naming the line number - a half-understood manifest is worse than none.

## 12. Non-goals

- No fallback when symlink creation is denied.
- No argument rewriting, environment filtering or output capture.
- No PATH editing; `path` only prints the directory.
- No version management of the targets, no update checks.
- No shim for directories or for anything without an `.exe` extension.
- No arm64 targets: the embedded shim is x64, so such a shim would fail at launch.

## 13. Embedding upstream artifacts

`assets/` holds the unmodified release binaries. `asset.go` pins repo, tag, commit and both
digests; `verifyEmbedded` re-checks them on every start, so a build that embedded the wrong
file fails before touching the disk.

Why not build locally: a self-compiled Rust/MSVC binary is a byte stream nobody else can
vouch for. Toolchain, base image and linker decide the hash, so there is nothing to compare
against. The upstream artifacts carry a SLSA provenance statement binding these exact
digests to a tag, a commit and a GitHub-hosted workflow run.

Updating the pinned version is therefore a four-line change (repo tag, commit, two
digests) plus replacing two files - and a rebuild that gets it wrong refuses to start.

## 14. Acceptance checklist

Each item was executed against a real tree; `[x]` means observed, not assumed.

| # | Check | Expected |
| --- | --- | --- |
| 1 | `jig version` without a root | prints digests, exit 0 |
| 2 | `jig ls` in a tree with no marker | exit 2, names all three sources tried |
| 3 | `jig init --root <new>` | creates `.jig-root`, `bin\_ref`, `templates` |
| 4 | `jig add` run from `<root>\bin` without a flag | marker found, works |
| 5 | shim execution | forwards argv, exit code and stdout |
| 6 | GUI target | `wshim-w.exe` shim, subsystem `gui` |
| 7 | console target | `wshim-c.exe` shim, subsystem `console` |
| 8 | `add` twice with the same target | idempotent, exit 0 |
| 9 | `add` same name, different target | exit 3 without `--force` |
| 10 | `add bad:name` / `add con` | exit 3, name rejected |
| 11 | `add` a target inside `bin` | exit 3, cycle refused |
| 12 | `rm` an unregistered name | exit 3, nothing deleted |
| 13 | foreign executable in `bin` | `warn foreign`, never deleted by `--fix` |
| 13b | `add` onto a name a foreign executable occupies | exit 3, file and `_ref` untouched; `--force` replaces |
| 14 | relative `_ref` entry | `warn relative`, `--fix` rewrites absolute, shim still runs |
| 15 | `jig.exe` inside `bin` | absent from `check` output, survives `--fix` |
| 16 | `--root` vs `$JIG_ROOT` | `--root` wins |
| 17 | `check` on a healthy tree | exit 0 |
| 18 | non-ASCII in `*.go` | none; all source and messages are ASCII |
| 19 | a shim after `add` | same file index as its template, still runs |
| 19b | template truncated in place | every shim reported, `--fix` restores and relinks them |
| 20 | `add` at the 1024-link ceiling | template rolls to a new inode, shim is still a link |
| 21 | `add` a shim template as the target | exit 3, cycle refused |
| 22 | `add` an unchanged entry twice | no new manifest event |
| 23 | `add <target>` without a name | shim name is the target basename |
| 24 | hold `.lock`, then `check --fix` / `check` | `--fix` exits 5; plain check still runs |
| 25 | unregistered copy of a template in `bin` | reported, never deleted by `--fix` |
| 26 | `add` through a `current` junction, then retarget it | stored target keeps `current`, shim follows |
