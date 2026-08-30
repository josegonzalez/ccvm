# This machine is a ccvm session

You are running inside a disposable machine created by `ccvm`. It exists for
this session only, and nothing here is backed up.

## Ending the session

Run `ccvm-done` when the work is finished. It destroys the machine.

- `ccvm-done` - end the session and destroy the machine.
- `ccvm-done --keep` - detach but leave the machine running, so `ccvm attach`
  can return to it later.
- `ccvm-done --force` - destroy even when there is uncommitted or unpushed
  work. Without it, `ccvm-done` refuses and tells you what would be lost.

Prefer committing and pushing before ending the session. `ccvm-done` checks for
work that would be lost, but it only knows about git.

## What is here

Your code is in `/work`. The machine is disposable, so treat anything outside
`/work` as scratch space.
