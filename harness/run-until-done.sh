#!/usr/bin/env bash
# Drive `gralph run lifecycle.yaml` until DONE, surviving Claude usage limits.
# Run from the harness dir:  ./run-until-done.sh
#
# Why an outer loop instead of a GALP `launcher:` -- on Windows the dist launcher never
# receives the prompt (the argv round-trip strips {{prompt}}'s braces) and its rate-limit
# pattern both misses the real banner and false-positives on a build's own output. gralph's
# own give-up (5 abnormal exits without cursor progress) is a SAFE stop -- state and store
# are preserved -- so all this needs to do is notice WHY it stopped and resume.
#
# The orchestrator is the PINNED PRE-FIX binary on purpose: Windows locks a running .exe,
# so the loop must not be executing the file the agent is rebuilding. See HARNESS.md.
set -u
cd "$(dirname "$0")" || exit 1

GRALPH="${GRALPH:-./bin/gralph-orch.exe}"
[ -x "$GRALPH" ] || { echo "orchestrator missing: $GRALPH"; exit 127; }

# Nested `claude -p` sessions refuse to start when CLAUDECODE is inherited from a parent
# Claude Code session; Korean Windows (cp949) needs PYTHONUTF8 for subprocess output.
unset CLAUDECODE
export PYTHONUTF8=1

PROFILE="${PROFILE:-lifecycle.yaml}"
DIR="${DIR:-.gralph/lifecycle}"
LOG="${LOG:-$DIR/run-until-done.log}"
COOLDOWN="${COOLDOWN:-1800}"     # sleep when a usage limit is what stopped us
ITERS="${ITERS:-12}"             # gralph iterations per round (per-round seatbelt)
MAX_ROUNDS="${MAX_ROUNDS:-40}"
STALL_LIMIT="${STALL_LIMIT:-2}"  # consecutive same-cursor rounds before handing to a human

# Anchored on the Claude CLI's limit banner. Deliberately NOT matching bare
# "quota"/"rate limit"/"429": this build's own output prints those words.
LIMIT_RE="hit your [a-z0-9-]+ limit|usage limit reached|rate limit exceeded|Claude usage limit"

mkdir -p "$DIR"
cursor() { python -c "import json;print(json.load(open('$DIR/state.json'))['cursor'])" 2>/dev/null || echo "?"; }

# The loop's OWN bookkeeping must reach the log file, not just stdout. A detached run
# has no console anyone reads, so without this the round transitions, the stall count,
# the usage-limit sleeps and the final verdict are simply lost -- measured: a `gralph
# run` exited silently mid-build and the log showed only the next round starting, with
# no way to tell a kill from a give-up.
say() { printf '%s\n' "$*" | tee -a "$LOG"; }

# Completion alarm -- the loop and any interactive session are separate processes, so
# nothing surfaces DONE/STUCK on its own. Bell + Windows toast (BurntToast -> msg * -> bell).
notify() { printf '\a'; powershell -NoProfile -Command "New-BurntToastNotification -Text '$1','$2'" >/dev/null 2>&1 || msg '*' "$1: $2" >/dev/null 2>&1 || true; }

stall=0
for round in $(seq 1 "$MAX_ROUNDS"); do
  [ "$(cursor)" = DONE ] && { echo "[loop] cursor=DONE after $((round-1)) round(s)"; notify "gralph lifecycle DONE" "flow complete"; exit 0; }

  before="$(cursor)"
  rlog="$(mktemp)"
  "$GRALPH" run "$PROFILE" --max-iterations "$ITERS" 2>&1 | tee -a "$LOG" | tee "$rlog"
  rc="${PIPESTATUS[0]}"
  after="$(cursor)"
  # The exit code is the difference between "gave up" (1), "stopped/done" (0) and
  # "was killed" (anything else, printed with no message of its own).
  say "[loop] round $round: cursor $before -> $after (gralph exit=$rc)"

  if [ "$after" = DONE ]; then echo "[loop] cursor=DONE"; rm -f "$rlog"; notify "gralph lifecycle DONE" "flow complete after $round round(s)"; exit 0; fi

  if grep -qiE "$LIMIT_RE" "$rlog"; then
    rm -f "$rlog"
    echo "[loop] stopped by a usage limit; sleeping ${COOLDOWN}s then resuming"
    sleep "$COOLDOWN"
    stall=0                      # a limit is not a stall
    continue
  fi
  rm -f "$rlog"

  if [ "$after" != "$before" ]; then stall=0; else stall=$((stall + 1)); fi
  if [ "$stall" -ge "$STALL_LIMIT" ]; then
    echo "[loop] $stall rounds with the cursor stuck at $after and no usage limit — a gate is genuinely stuck."
    echo "[loop] Inspect $LOG and $DIR/failures.json; not looping further."
    notify "gralph lifecycle STUCK" "gate stalled at cursor=$after; needs a human"
    exit 1
  fi
done

echo "[loop] hit MAX_ROUNDS=$MAX_ROUNDS without reaching DONE (cursor=$(cursor))"
notify "gralph lifecycle TIMEOUT" "hit MAX_ROUNDS=$MAX_ROUNDS, cursor=$(cursor)"
exit 1
