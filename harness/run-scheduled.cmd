@echo off
REM Start the loop under the Windows Task Scheduler service, which is the only launch
REM path measured to survive on this machine.
REM
REM Why not just run-detached.cmd: launching it from a Claude Code Bash tool call --
REM even through `start`, which gives the child its own console -- did NOT detach it.
REM Twice, the ENTIRE tree (bash + gralph + agent) vanished mid-session with no message
REM anywhere: once ~35 minutes in, once ~6 minutes in while the agent was actively
REM writing code. The outer loop never reached its own post-run log line, so it was
REM killed rather than having exited. `start` does not break out of a job object.
REM
REM Under Task Scheduler the ancestry becomes
REM   gralph-orch.exe <- bash <- bash <- bash <- cmd.exe <- svchost.exe <- services.exe
REM which is outside any session's tree. Verify that chain after launching; if you see
REM claude.exe or a Claude Code shell in it, you are back in the failing configuration.
REM
REM /IT runs it in the interactive session so `claude -p` keeps the user's credentials.
REM The trigger is DISABLED immediately after /Run on purpose: /SC ONCE leaves a live
REM trigger that would start a SECOND orchestrator on the same instance later, which is
REM exactly the concurrent-state corruption this harness exists to remove.

set TASK=gralph-lifecycle
schtasks /Create /TN "%TASK%" /TR "%~dp0run-detached.cmd" /SC ONCE /ST 23:59 /RU "%USERNAME%" /IT /F || exit /b 1
schtasks /Run /TN "%TASK%" || exit /b 1
schtasks /Change /TN "%TASK%" /DISABLE >nul
echo Started under Task Scheduler. Verify ancestry ends in svchost.exe/services.exe:
echo   powershell -NoProfile -Command "Get-CimInstance Win32_Process ^| Where-Object { $_.Name -eq 'gralph-orch.exe' }"
