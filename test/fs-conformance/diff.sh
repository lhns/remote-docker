#!/usr/bin/env bash
# diff.sh compares two fsprobe transcripts, step by step, against a list of
# the differences that are allowed.
#
#   diff.sh <native> <share> <deviations>
#
# A step id is the text before the first `: ` on a line: `<group>/<step>`,
# with a `.<sub>` suffix on a step that prints several lines.
#
# For each id present in both transcripts, one of three outcomes:
#   identical    fine.
#   different    EXPLAINED if the share's line appears verbatim in the
#                deviations file, UNEXPLAINED if not.
#   in one only  MISSING.
# A deviations entry that no step needed is STALE. Unexplained, stale or
# missing exits 1.
#
# The deviations file: blank lines and `#` lines are comments, everything else
# is a share-side transcript line. A `# reason (pointer)` comment covers every
# entry under it until the next blank line, so a class of deviations is one
# comment and a block. An entry with no comment above its block is warned
# about and not failed, because the report is what somebody fills the file
# from.
#
# An entry is COPIED from `suggested-deviations.txt`, written beside the share
# transcript, and never typed: it holds the unexplained share lines verbatim
# under `# TODO` lines, and the stale entries commented out. The report's own
# pairs carry a `share:  ` prefix and are not pasteable.
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

# Beside the share transcript, which is where the caller puts the report.
# Removed first, so a run that has nothing to fix leaves no stale suggestion
# from the run before it.
suggested=$(dirname "$share")/suggested-deviations.txt
rm -f "$suggested"

# One awk pass over the three files, told apart by name. FNR==NR only
# distinguishes the first, so the names are passed in.
awk -v native="$native" -v share="$share" -v deviations="$deviations" -v suggested="$suggested" '
function stepid(line,    i) {
    i = index(line, ": ")
    return i ? substr(line, 1, i - 1) : ""
}

FILENAME == native {
    id = stepid($0)
    if (id == "") next
    if (id in nat) warn[++nwarn] = "native names step " id " twice"
    else norder[++nn] = id
    nat[id] = $0
    next
}

FILENAME == share {
    id = stepid($0)
    if (id == "") next
    if (id in shr) warn[++nwarn] = "share names step " id " twice"
    else sorder[++ns] = id
    shr[id] = $0
    next
}

# A comment opens a block and a blank line closes it, so consecutive entries
# under one comment are all covered by it.
FILENAME == deviations {
    if ($0 ~ /^[[:space:]]*$/) { in_block = 0; next }
    if ($0 ~ /^#/) { in_block = 1; next }
    if (!in_block) warn[++nwarn] = "deviation on line " FNR " has no # reason comment above it: " $0
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
        if (nat[id] == shr[id]) continue
        differ++
        if (shr[id] in dev) { used[shr[id]] = 1; continue }
        unex[++nun] = id
    }
    for (i = 1; i <= ns; i++) {
        id = sorder[i]
        if (!(id in nat)) missing[++nmissing] = "in share only:  " shr[id]
    }
    for (i = 1; i <= nd; i++) {
        if (!(dorder[i] in used)) stale[++nstale] = dorder[i]
    }

    printf "steps compared: %d   identical: %d   different: %d (explained %d, unexplained %d)\n",
        compared, compared - differ, differ, differ - nun, nun
    printf "deviations listed: %d   stale: %d   missing steps: %d\n", nd, nstale, nmissing

    if (nun) {
        print ""
        print "unexplained:"
        for (i = 1; i <= nun; i++) {
            print "native: " nat[unex[i]]
            print "share:  " shr[unex[i]]
        }
    }
    if (nstale) {
        print ""
        print "stale (listed in " deviations ", no longer observed):"
        for (i = 1; i <= nstale; i++) print "line " dev[stale[i]] ": " stale[i]
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
        for (i = 1; i <= nun; i++) print unex[i]
    }

    if (nun || nstale) {
        print "# Written by test/fs-conformance/diff.sh from this run." > suggested
        print "# Copy an entry into " deviations " once its TODO names a real" > suggested
        print "# reason and a pointer. The transcript lines are verbatim and must stay so." > suggested
        for (i = 1; i <= nun; i++) {
            print "" > suggested
            print "# TODO reason (pointer)" > suggested
            print shr[unex[i]] > suggested
        }
        if (nstale) {
            print "" > suggested
            print "# No longer observed. Delete these from " deviations ":" > suggested
            for (i = 1; i <= nstale; i++) print "#" stale[i] > suggested
        }
        close(suggested)
        print ""
        print "suggestions written to " suggested
    }

    exit (nun || nstale || nmissing) ? 1 : 0
}
' "$native" "$share" "$deviations"
