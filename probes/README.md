# probes

Reproductions for behaviour that unit tests cannot show on their own, kept so the
reasoning behind the code can be checked rather than taken on trust.

None of these runs in CI. They are here to be run by hand when the code they
cover is changed, and to record what was measured when it was written.

| | What it answers |
|---|---|
| `pid1-zombies.sh` | Does supavisor leak zombies when it is PID 1? |
| `completed-latch.sh` | Can a dependent of a finished one-off still be started? |
| `wait4-race/main.go` | Why does supavisor not simply add a `wait4(-1)` goroutine? |

## pid1-zombies.sh

Runs supavisor as PID 1 in a container, supervising a program that backgrounds a
grandchild and exits — the wrapper-script shape that orphans processes onto PID 1.
Samples the zombie count, then prints the whole process table.

```bash
./probes/pid1-zombies.sh              # supavisor as PID 1, which is the case under test
./probes/pid1-zombies.sh --init       # control: docker-init as PID 1, supavisor below it
```

The supervised program is `sh -c "tail -f /dev/null & exec sleep 2"`. The direct
child is the `sleep` and the grandchild is the `tail`, deliberately different
commands: supavisor is PID 1 here, so its own direct child also has `ppid=1`, and
parentage alone cannot tell a running program from a leaked orphan. The command can.
At most one `(tail)` should exist at a time, and only while its run is in flight.

A correct run, sampled across three restarts:

```
t=8s   zombies: 0  grandchildren: 1
t=16s  zombies: 0  grandchildren: 0
t=24s  zombies: 0  grandchildren: 1

  pid=1   state=S ppid=0  cmd=(supavisor)
  pid=77  state=S ppid=1  cmd=(sleep)      <- the run in flight
  pid=78  state=S ppid=77 cmd=(tail)       <- its grandchild, correctly parented
```

The same script against the code before the reaper existed:

```
t=24s  zombies: 4  grandchildren: 5

  pid=1   state=S ppid=0  cmd=(supavisor)
  pid=17  state=Z ppid=1  cmd=(tail)       <- killed, never reaped
  pid=24  state=Z ppid=1  cmd=(tail)
  pid=26  state=Z ppid=1  cmd=(tail)
  pid=57  state=Z ppid=1  cmd=(tail)
  pid=87  state=S ppid=1  cmd=(sleep)
  pid=88  state=S ppid=87 cmd=(tail)
```

The `(sh)` with `ppid=0` in real output is the `docker exec` doing the sampling,
not a leak.

**Read the process table, not only the count.** Zero zombies is necessary but not
sufficient. While writing the reaper, an ordering mistake released the `os.Process`
handle before reading its PID — `Release` sets it to -1 — so `killLingeringGroup`
was handed -1 and bailed on its own guard. The zombie count was zero and the run
looked clean. The grandchildren were simply still *alive*, `state=S`, never killed:

```
t=35s  zombies: 0  grandchildren: 5       <- count fine, table not

  pid=17 state=S ppid=1 cmd=(tail)        <- should have gone with its group
  pid=19 state=S ppid=1 cmd=(tail)
```

That is what the `grandchildren` column is for: it counts them whether they are
zombies or alive.

## completed-latch.sh

Runs a one-off task and a program that waits on it, in a container, and asks the
two questions `condition: completed` exists to answer: does the dependent start
*after* the work rather than alongside it, and can it still be started once the
task has been sitting in `EXITED` for a while?

```bash
./probes/completed-latch.sh              # condition: completed, the case under test
./probes/completed-latch.sh --started    # control: condition: started instead
```

The second phase is the one a unit test cannot really show. It leaves the
dependent up for 20 seconds after the task finished, then takes it down and starts
it again — a crash an hour later, compressed. That is the case that used to be
unrecoverable for the lifetime of the daemon.

A correct run:

```
t=1s  migrate=RUNNING  api=STOPPED
t=4s  migrate=RUNNING  api=STOPPED     <- did not start alongside the work
t=5s  migrate=EXITED   api=RUNNING     <- started once the work was done

  sctl stop api ... ok
  sctl start api ... ok (1s)           <- the completion outlived the run

NAME      STATE     DESIRED   HEALTH   PID   EXIT_CODE   RESTARTS   UPTIME
api       RUNNING   RUNNING   -        210   -1          0          1s
migrate   EXITED    RUNNING   -        N/A   0           0          N/A
```

The control, which is what the same configuration did before the condition
existed, fails in both phases:

```
t=2s  migrate=RUNNING  api=RUNNING     <- ran alongside the task, not after it
t=5s  migrate=EXITED   api=RUNNING

  sctl stop api ... ok
  sctl start api ... REFUSED (30s)     <- and now it cannot come back

Why api is not running: dependency migrate is EXITED
```

**The second phase is the point, not the first.** `condition: started` letting the
dependent run alongside the task is visible immediately and reads as a
configuration mistake. The refusal 25 seconds later is the one that surfaces in
production, long after the config was written, and looks nothing like one.

## wait4-race/main.go

The reason `internal/process/reaper.go` exists in the shape it does, rather than as
a reaper goroutine bolted onto code that still calls `cmd.Wait()`.

`os/exec` waits on a specific PID. A concurrent `wait4(-1)` takes whatever has
exited, including that PID, and then `cmd.Wait()` finds nothing left to wait for.
The exit code is gone.

```bash
go run probes/wait4-race/main.go
```

Measured when this was written, 200 children each exiting 7:

```
exit status survived: 194/200, LOST: 6/200
```

Every loss is an exit code becoming -1. Under `autorestart: unexpected` that turns
a clean exit into an apparent crash, and `max_restarts` eventually parks the program
in `FATAL`. At around 3% it is rare enough to survive a test run and reappear in
production.

This is why there is exactly one waiter in the daemon, and why nothing outside
`internal/process/reaper.go` calls `wait4` or `cmd.Wait()`.
