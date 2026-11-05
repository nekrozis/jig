#!/usr/bin/env bash
# Acceptance checks for jig, mirroring SPEC.md section 14.
# Requires Git Bash (or an equivalent POSIX shell) on Windows.
#
# Usage: test/acceptance.sh [path-to-jig.exe]
set -u

here="$(cd "$(dirname "$0")/.." && pwd)"
JIG="${1:-$here/jig.exe}"
[ -x "$JIG" ] || { echo "jig not found at $JIG (build it first)" >&2; exit 1; }

pass=0
fail=0
chk() { # chk <label> <expected> <actual>
	if [ "$2" = "$3" ]; then
		printf 'PASS  %s\n' "$1"
		pass=$((pass + 1))
	else
		printf 'FAIL  %s\n        expected: %s\n        actual:   %s\n' "$1" "$2" "$3"
		fail=$((fail + 1))
	fi
}
contains() { # contains <label> <needle> <haystack>
	case "$3" in
	*"$2"*) chk "$1" "yes" "yes" ;;
	*) chk "$1" "yes" "no" ;;
	esac
}

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
R="$work/root"

# 1. version works with no root at all
out="$("$JIG" version 2>&1)"
chk "1  version without a root exits 0" "0" "$?"
contains "1  version prints the pinned digest" "65027fea0dd526e98301184e070468adf9352d7cd99e0ff0af572a640fe488e3" "$out"

# 2. no marker anywhere above the executable -> usage error
mkdir -p "$work/lone"
cp "$JIG" "$work/lone/jig.exe"
out="$(cd "$work/lone" && ./jig.exe ls 2>&1)"
chk "2  no root is a usage error" "2" "$?"
contains "2  the error names the marker" ".jig-root" "$out"

# 3. init plants the skeleton and the marker
"$JIG" init --root "$R" >/dev/null 2>&1
chk "3  init exits 0" "0" "$?"
chk "3  marker written" "yes" "$([ -f "$R/.jig-root" ] && echo yes || echo no)"
chk "3  ref directory created" "yes" "$([ -d "$R/bin/.ref" ] && echo yes || echo no)"

# 4. jig installed inside bin finds the root by marker
cp "$JIG" "$R/bin/jig.exe"
bin="$R/bin"
out="$(cd "$bin" && ./jig.exe add whoami 'C:\Windows\System32\whoami.exe' 2>&1)"
chk "4  add without --root from inside bin" "0" "$?"

# 5. the shim forwards
out="$("$bin/whoami.exe" 2>&1)"
chk "5  shim exits 0" "0" "$?"
contains "5  shim forwards stdout" "\\" "$out"

# 6/7. template chosen by subsystem
"$JIG" --root "$R" add np 'C:\Windows\System32\notepad.exe' >/dev/null 2>&1
cui_sum="$(sha256sum "$bin/whoami.exe" | cut -d' ' -f1)"
gui_sum="$(sha256sum "$bin/np.exe" | cut -d' ' -f1)"
chk "6  console target uses wshim-c.exe" "65027fea0dd526e98301184e070468adf9352d7cd99e0ff0af572a640fe488e3" "$cui_sum"
chk "7  gui target uses wshim-w.exe" "b3cdb49fbbeba420de0b85a4d121623661ec5b285ef1825d5741950501cf8deb" "$gui_sum"

# 8. idempotent re-add
"$JIG" --root "$R" add whoami 'C:\Windows\System32\whoami.exe' >/dev/null 2>&1
chk "8  re-adding the same target exits 0" "0" "$?"

# 9. conflicting target
"$JIG" --root "$R" add whoami 'C:\Windows\System32\notepad.exe' >/dev/null 2>&1
chk "9  same name, different target is refused" "3" "$?"

# 10. bad names
"$JIG" --root "$R" add 'bad:name' 'C:\Windows\System32\whoami.exe' >/dev/null 2>&1
chk "10 illegal character in name rejected" "3" "$?"
"$JIG" --root "$R" add con 'C:\Windows\System32\whoami.exe' >/dev/null 2>&1
chk "10 reserved device name rejected" "3" "$?"

# 11. cycle
"$JIG" --root "$R" add loop "$bin/whoami.exe" >/dev/null 2>&1
chk "11 target inside bin is a cycle" "3" "$?"

# 12. rm of an unknown name
"$JIG" --root "$R" rm nosuch >/dev/null 2>&1
chk "12 rm refuses an unregistered name" "3" "$?"

# 13. foreign executable survives --fix
cp /c/Windows/System32/notepad.exe "$bin/foreign.exe"
out="$("$JIG" --root "$R" check --fix 2>&1)"
contains "13 foreign file is reported" "foreign" "$out"
chk "13 foreign file is not deleted" "yes" "$([ -f "$bin/foreign.exe" ] && echo yes || echo no)"
rm -f "$bin/foreign.exe"

# 14. relative link is rewritten
"$JIG" --root "$R" rm np >/dev/null 2>&1
rm -f "$R/bin/.ref/whoami.exe"
# Python is a native Windows build: hand it a Windows cwd and let it compute the
# relative path, instead of guessing how many ".." reach C:\.
(cd "$bin" && python -c "
import os, sys
d = os.path.abspath(sys.argv[1])
os.symlink(os.path.relpath(r'C:\Windows\System32\whoami.exe', d), os.path.join(d, 'whoami.exe'))
" '.ref') || echo "      (python unavailable, relative-link checks skipped)"
out="$("$JIG" --root "$R" check 2>&1)"
contains "14 relative link detected" "relative" "$out"
"$JIG" --root "$R" check --fix >/dev/null 2>&1
out="$("$JIG" --root "$R" check 2>&1)"
chk "14 relative link repaired" "yes" \
	"$(echo "$out" | grep -qE '^\[ok\][[:space:]]+whoami' && echo yes || echo no)"
chk "14 shim still runs after the repair" "0" "$("$bin/whoami.exe" >/dev/null 2>&1; echo $?)"

# 15. jig.exe is invisible to check and survives --fix
out="$("$JIG" --root "$R" check 2>&1)"
chk "15 jig.exe absent from check output" "no" "$(echo "$out" | grep -qi ' jig ' && echo yes || echo no)"
"$JIG" --root "$R" check --fix >/dev/null 2>&1
chk "15 jig.exe survives check --fix" "yes" "$([ -f "$bin/jig.exe" ] && echo yes || echo no)"

# 16. --root wins over the environment
want="$(cygpath -w "$bin")"
out="$(JIG_ROOT="$work/nope" "$JIG" --root "$R" path 2>&1)"
chk "16 --root beats JIG_ROOT" "$want" "$out"
out="$(JIG_ROOT="$R" "$JIG" path 2>&1)"
chk "16 JIG_ROOT is honoured" "$want" "$out"

# 17. healthy tree
"$JIG" --root "$R" check >/dev/null 2>&1
chk "17 healthy tree checks clean" "0" "$?"

# 18. source is pure ASCII
nonascii="$(grep -lP '[^\x00-\x7F]' "$here"/*.go 2>/dev/null | wc -l | tr -d ' ')"
chk "18 all .go sources are ASCII" "0" "$nonascii"

# 19. a shim shares one inode with its template instead of copying the bytes
"$JIG" --root "$R" add linkprobe 'C:\Windows\System32\hostname.exe' >/dev/null 2>&1
chk "19 shim shares the template's file" "same" "$(
	i1="$(stat -c %i "$bin/linkprobe.exe" 2>/dev/null)"
	i2="$(stat -c %i "$R/templates/wshim-c.exe" 2>/dev/null)"
	[ -n "$i1" ] && [ "$i1" = "$i2" ] && echo same || echo different
)"
chk "19 linked shim still runs" "0" "$("$bin/linkprobe.exe" >/dev/null 2>&1; echo $?)"

# 19b. shared bytes means an in-place write to the template reaches every shim;
#      that cannot be prevented, so recovery is the contract being tested.
python -c "import sys; f=open(sys.argv[1],'r+b'); f.write(b'X'*64); f.truncate(900); f.close()" \
	"$R/templates/wshim-c.exe" 2>/dev/null
out="$("$JIG" --root "$R" check 2>&1)"
contains "19b damaged template is detected" "differs" "$out"
"$JIG" --root "$R" check --fix >/dev/null 2>&1
chk "19b every shim was restored" "0" "$("$JIG" --root "$R" check >/dev/null 2>&1; echo $?)"
chk "19b restored shim runs" "0" "$("$bin/linkprobe.exe" >/dev/null 2>&1; echo $?)"
chk "19b restoration is a link again" "same" "$(
	i1="$(stat -c %i "$bin/linkprobe.exe" 2>/dev/null)"
	i2="$(stat -c %i "$R/templates/wshim-c.exe" 2>/dev/null)"
	[ -n "$i1" ] && [ "$i1" = "$i2" ] && echo same || echo different
)"

# 20. NTFS stops a file at 1024 links. That must roll the template onto a fresh
#     inode, not silently degrade every later shim to a 310 KB copy.
python - "$R/templates/wshim-c.exe" <<'PY' 2>/dev/null
import os, sys
d = sys.argv[1]
i = 0
while True:
    try:
        os.link(d, os.path.join(os.path.dirname(d), 'pad%04d.exe' % i))
        i += 1
    except OSError:
        break
PY
chk "20 the template really is at the ceiling" "yes" "$(
	[ "$(stat -c %h "$R/templates/wshim-c.exe")" -ge 1024 ] && echo yes || echo no
)"
"$JIG" --root "$R" add ceilprobe 'C:\Windows\System32\hostname.exe' >/dev/null 2>&1
chk "20 add at the ceiling succeeds" "0" "$?"
chk "20 the ceiling rolled the template to a new inode" "different" "$(
	[ "$(stat -c %i "$R/templates/wshim-c.exe")" = "$(stat -c %i "$R/templates/pad0000.exe")" ] &&
		echo same || echo different
)"
chk "20 the new shim is a link, not a copy" "same" "$(
	i1="$(stat -c %i "$bin/ceilprobe.exe" 2>/dev/null)"
	i2="$(stat -c %i "$R/templates/wshim-c.exe" 2>/dev/null)"
	[ -n "$i1" ] && [ "$i1" = "$i2" ] && echo same || echo different
)"
chk "20 shim created at the ceiling runs" "0" "$("$bin/ceilprobe.exe" >/dev/null 2>&1; echo $?)"

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
