# Minecraft server in tmux

Scripts for running a Minecraft Java server (vanilla, Paper, Purpur,
Fabric, ...) with Server Manager's tmux backend:

| File | Purpose |
|---|---|
| `start.sh` | Runs inside the tmux session. Checks Java, the jar and the EULA, then starts the server. |
| `backup.sh` | Consistent world backups: pauses saving, flushes, archives, resumes. Works with the server running or stopped. |
| [`../servers/minecraft.json`](../servers/minecraft.json) | Matching server definition. |

Server Manager creates the tmux session itself and runs `start.sh` in it, so
the script does not start tmux. It replaces itself with Java, so when
Minecraft stops, the session closes and the server shows as STOPPED.

## Setup

All commands below assume the server lives in `/opt/minecraft` and runs as
the same user as Server Manager (that user must own the tmux session).

1. Put the server jar in `/opt/minecraft` and name it `server.jar` (or set
   `SERVER_JAR`). Minecraft 1.20.5 and newer need Java 21.
2. Copy `start.sh` and `backup.sh` there and make them executable:

   ```bash
   chmod +x /opt/minecraft/start.sh /opt/minecraft/backup.sh
   ```

3. Read the [Minecraft EULA](https://aka.ms/MinecraftEULA). If you agree:

   ```bash
   echo eula=true > /opt/minecraft/eula.txt
   ```

4. Copy `minecraft.json` to `/etc/server-manager/servers/`, adjust paths and
   memory, then use **Settings → Reload from disk** (or
   `sudo systemctl reload server-manager`). You can also enter the same
   values in the web form under **Servers → Add server**.

## Settings

Set these in the definition's `"env"` section:

| Variable | Default | Used by |
|---|---|---|
| `SERVER_JAR` | `server.jar` | start.sh |
| `MIN_MEMORY` / `MAX_MEMORY` | `2G` / `4G` | start.sh (`-Xms` / `-Xmx`); leave memory for the OS |
| `JAVA_BIN` | `java` | start.sh; full path to pick a specific Java version |
| `EXTRA_JAVA_ARGS` | *(empty)* | start.sh; additional JVM flags |
| `TMUX_SESSION` | `minecraft` | backup.sh; must equal `tmux.session` |
| `WORLDS` | `world world_nether world_the_end` | backup.sh |
| `KEEP_BACKUPS` | `14` | backup.sh; older archives are deleted |
| `SAVE_TIMEOUT` | `120` | backup.sh; seconds to wait for "Saved the game" |

## How the pieces fit

- **Ready detection:** the server counts as RUNNING once the console shows
  `Done (…s)! For help`.
- **Stop:** Server Manager types `stop`. If the server is still running after
  `timeouts.stop`, the session is killed.
- **Console:** commands typed in the web console go straight to the server
  console. Leave off the leading `/`, e.g. `say hello`.
- **Backups:** **Create backup** runs `backup.sh`. Archives appear in the
  Backups tab and can be downloaded there.
- **Without Server Manager:** the scripts work on their own too:

  ```bash
  tmux new-session -d -s minecraft /opt/minecraft/start.sh
  ```
