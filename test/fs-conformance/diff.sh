#!/usr/bin/env bash
# diff.sh compares two fsprobe transcripts, step by step, against a list of
# the differences that are allowed.
#
#   diff.sh <native> <share> <deviations>
#
# A step id is the text before the first `: ` on a line: `<group>/<step>`,
# with a `.<sub>` suffix on a step that prints several lines. fsprobe gives
# every line its own, so a repeated id is a probe bug and is warned about.
# For each id present in both transcripts: identical is fine; different is EXPLAINED if the
# share's line appears verbatim in the deviations file and UNEXPLAINED if not.
# A deviations entry that no step needed is STALE. A step in one transcript
# and not the other is MISSING. Unexplained, stale or missing exits 1.
#
# The deviations file: blank lines and `#` lines are comments, everything
# else is a share-side transcript line. The line above an entry is expected
# to be a `# reason (pointer)` comment; one without it is warned about and
# not failed, because the report is what somebody fills the file from.
#
# The report prints the counts first, then the unexplained steps as
# `native:` / `share:` pairs, then the stale entries, then the missing steps,
# and last the ids of the unexplained steps on their own, one per line, to be
# copied into the deviations file without the stat noise.
set -uo pipefail

if [ $# -ne 3 ]; then
    echo "usage: $0 <native-transcript> <share-transcript> <deviations>" >&2
    exit 2
fi
native=$1 share=$2 deviations=$3
for f in "$native" "$share" "$deviations"; do
    if [ ! -f "$f" ]; then
        echo "diff.sh: no such file: $f" >&2
        exit 2
    fi
done

# One awk pass over the three files, told apart by name. FNR==NR only
# distinguishes the first, so the names are passed in.
awk -v native="$native" -v share="$share" -v deviations="$deviations" '
function stepid(line,    i) {
    i = index(line, ": ")
    return i ? substr(line, 1, i - 1) : ""
}

FILENAME == native {
    id = stepid($0)
    if (id == "") { unparsed++; next }
    if (id in nat) warn[++nwarn] = "native names step " id " twice"
    else norder[++nn] = id
    nat[id] = $0
    next
}

FILENAME == share {
    id = stepid($0)
    if (id == "") { unparsed++; next }
    if (id in shr) warn[++nwarn] = "share names step " id " twice"
    else sorder[++ns] = id
    shr[id] = $0
    next
}

FILENAME == deviations {
    if ($0 ~ /^[[:space:]]*$/) { prev_comment = 0; next }
    if ($0 ~ /^#/) { prev_comment = 1; next }
    if (!prev_comment) warn[++nwarn] = "deviation on line " FNR " has no # reason comment above it: " $0
    prev_comment = 0
    if ($0 in dev) warn[++nwarn] = "deviation on line " FNR " is listed twice"
    else dorder[++nd] = $0
    dev[$0] = FNR
    next
}

END {
    for (i = 1; i <= nn; i++) {
        id = norder[i]
        if (!(id in shr)) { missing[++nmissing] = "in native only: " nat[id]; continue }
        compared++
        if (nat[id] == shr[id]) { same++; continue }
        differ++
        if (shr[id] in dev) { explained++; used[shr[id]] = 1; continue }
        unexplained[++nun] = "native: " nat[id] "\nshare:  " shr[id]
        unexplained_id[nun] = id
    }
    for (i = 1; i <= ns; i++) {
        id = sorder[i]
        if (!(id in nat)) missing[++nmissing] = "in share only:  " shr[id]
    }
    for (i = 1; i <= nd; i++) {
        if (!(dorder[i] in used)) stale[++nstale] = "line " dev[dorder[i]] ": " dorder[i]
    }

    printf "steps compared: %d   identical: %d   different: %d (explained %d, unexplained %d)\n",
        compared, same, differ, explained, nun
    printf "deviations listed: %d   stale: %d   missing steps: %d   unparsed lines: %d\n",
        nd, nstale, nmissing, unparsed

    if (nun) {
        print ""
        print "unexplained:"
        for (i = 1; i <= nun; i++) print unexplained[i]
    }
    if (nstale) {
        print ""
        print "stale (listed in " deviations ", no longer observed):"
        for (i = 1; i <= nstale; i++) print stale[i]
    }
    if (nmissing) {
        print ""
        print "missing:"
        for (i = 1; i <= nmissing; i++) print missing[i]
    }
    if (nwarn) {
        print ""
        print "warnings:"
        for (i = 1; i <= nwarn; i++) print warn[i]
    }
    if (nun) {
        print ""
        print "unexplained ids:"
        for (i = 1; i <= nun; i++) print unexplained_id[i]
    }
    exit (nun || nstale || nmissing) ? 1 : 0
}
' "$native" "$share" "$deviations"
