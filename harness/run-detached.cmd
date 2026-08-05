@echo off
REM Launch run-until-done.sh in a process tree that does NOT belong to the Claude Code
REM session. Measured three times on the sibling harnesses: a `run_in_background` bash
REM task is reaped when the session rotates, killing gralph mid-round (the script dies
REM during `gralph run`, so no "[loop] round N:" line is ever printed and the exit status
REM still reads 0).
REM
REM   run-detached.cmd                          <- from cmd
REM   MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*' cmd /c start "" /min run-detached.cmd
REM                                             <- from git-bash. The env vars are NOT
REM   optional: without them MSYS rewrites /min into C:/Program Files/Git/min and the
REM   launch fails with "cannot find C:/Program Files/Git/min" (measured).
REM
REM Progress goes to .gralph/lifecycle/run-until-done.log as usual. The loop rings
REM notify() (bell + Windows toast) at DONE / STUCK / TIMEOUT -- with no parent session
REM to re-invoke, that is the only completion signal a detached run gives you.
REM Set here as well as in run-until-done.sh because this is the outermost process the
REM whole loop inherits from, and the value is load-bearing: UTF-8 mode makes subprocess
REM text pipes decode child output as UTF-8, while `tasklist` emits the OEM codepage
REM (cp949 here). proc.py then dies on the Korean "no matching task" banner -- which
REM tasklist prints exactly when a PID is GONE, so the probes crash only when they would
REM have PASSED. An edit to run-until-done.sh cannot reach a shell that already started.
set PYTHONUTF8=0
REM Same reason this one lives here too: NoDefaultCurrentDirectoryInExePath=1 (set
REM machine-wide) removes the current directory from CreateProcess's search, so the
REM probe's fake agent cannot spawn `bin/gralph-uut.exe` and the shutdown proof blames
REM gralph for a WinError 2 it never caused. The empty value DELETES the variable;
REM setting it to 0 would still count as set.
set "NoDefaultCurrentDirectoryInExePath="
cd /d "%~dp0"
"C:\Program Files\Git\bin\bash.exe" run-until-done.sh
