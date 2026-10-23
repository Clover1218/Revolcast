# Revolcast Multi-Server Controller Demo

A small Go controller that starts multiple MCDR instances and multiplexes their stdin/stdout through one panel console.

## Important assumptions

- Each configured `work_dir` is an independent MCDR working directory.
- Each MCDR instance manages its own Minecraft server and QueQiao plugin/config.
- The controller sends normal input to MCDR's stdin. This preserves MCDR plugin commands such as `!!MCDR ...`.
- This demo does not directly start Minecraft. It starts the configured MCDR command.
- It does not use Mount or FIFO.
- `!!stop`/`!!quit` send `!!MCDR shutdown` and force-kill the MCDR process after 10 seconds if it remains alive. Test this behavior in a disposable environment first.
- A non-TTY panel may not support interactive input behavior exactly like a real terminal. The controller itself only reads newline-delimited stdin.

## Files

- `main.go`: controller source
- `controller.json`: example config
- `start.sh`: single entry point
- `controller-logs/`: generated per-instance controller output logs

## Configure

Edit `controller.json`:
- `name`: short unique target name
- `enabled`: whether this instance can be started
- `auto_start`: start automatically when controller starts
- `work_dir`: absolute path to that instance's MCDR directory
- `command` and `args`: exact executable and arguments used to launch MCDR
- `env`: extra environment variables
- `restart`: restart the MCDR process if it exits unexpectedly

Do not copy the example paths blindly. Set `command` to the actual Python/MCDR executable available on your hosting platform. If MCDR is installed in a virtual environment, use its Python path, for example `/home/container/venv/bin/python`.

## Run

```sh
chmod +x start.sh
./start.sh
```

If Go is unavailable in the host container, compile `controller` for the target Linux architecture elsewhere and upload it; then change `start.sh` to:

```sh
#!/bin/sh
set -eu
cd "$(dirname "$0")"
exec ./controller ./controller.json
```

## Console commands

- `!!help`
- `!!servers`
- `!!use element`
- `!!start afop`
- `!!stop afop`
- `!!restart afop`
- `!!logs element 100`
- `!!all`
- `!!current`
- `!!send afop list`
- `!!quit`

Any other line is sent unchanged to the selected MCDR instance. This includes MCDR plugin commands. Since `!!` is reserved for controller commands, use `!!send <name> <command>` to send a command beginning with `!!` to a specific MCDR.

## Limitations of this demo

- It is a demonstration, not a hardened supervisor.
- `!!stop` currently asks MCDR to shut down, then force-kills the MCDR process after a grace period. Confirm your MCDR version supports `!!MCDR shutdown` and adjust the shutdown command if needed.
- Logs are appended forever; add log rotation before long-term use.
- If the hosting panel restarts the entire script, the controller and its child processes may all be terminated by the platform.
- Make sure the hosting service permits several long-running child processes and enough memory/CPU for all instances.
- MCDR paths, Python paths, environment variables and plugin versions must be configured per server.


## Non-TTY hosting console behavior

This version does not print a prompt before every stdin read. Hosting panels may provide stdin/stdout without an interactive TTY or cursor; reprinting prompts while background logs arrive makes the display look corrupted. The controller prints the initial input target once, then prints only non-empty log lines. After `!!use <name>`, it prints the newly selected input target. Commands are still submitted through the panel's stdin.


## Non-TTY hosting console behavior

This version does not print a prompt before every stdin read. Hosting panels may provide stdin/stdout without an interactive TTY or cursor; reprinting prompts while background logs arrive makes the display look corrupted. The controller prints the initial input target once, then prints only non-empty log lines. After `!!use <name>`, it prints the newly selected input target. Commands are still submitted through the panel's stdin.
