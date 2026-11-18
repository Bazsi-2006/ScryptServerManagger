# Minecraft in tmux, running as the gameserver user

This setup runs a Minecraft server (vanilla, Paper, Fabric, ...) in a tmux
session managed by Server Manager, while the Java process itself runs as a
separate `gameserver` user:

```text
tmux session "minecraft"          owned by the Server Manager service user
  -> sudo -n -u gameserver /home/gameserver/minecraft/start.sh
       -> java -jar server.jar nogui   runs as gameserver, owns the world files
```

Server Manager never needs access to the Minecraft folder. It may run exactly
one command as `gameserver`: the start script, without arguments.

| File | Install as |
|---|---|
| [start.sh](start.sh) | `/home/gameserver/minecraft/start.sh` |
| [sudoers](sudoers) | `/etc/sudoers.d/server-manager-minecraft` |
| [minecraft.json](minecraft.json) | `<servers_dir>/minecraft.json`, or paste it into "Edit as JSON" in the web UI |

## Setup

1. Edit the settings block at the top of `start.sh` (folder, jar name,
   memory, Java path). Then install it so that only `gameserver` (or root)
   can change it:

   ```bash
   sudo install -m 0750 -o gameserver -g gameserver start.sh /home/gameserver/minecraft/start.sh
   ```

2. Allow the Server Manager service user to run it. Replace `server-manager`
   in the file if your service user has another name:

   ```bash
   sudo visudo -f /etc/sudoers.d/server-manager-minecraft
   ```

3. Check the rule as the service user. It must print the script's own error
   or start the server, not "a password is required":

   ```bash
   sudo -u server-manager sudo -n -u gameserver /home/gameserver/minecraft/start.sh
   ```

   Stop it again with Ctrl+C if it started.

4. Add the server definition (`minecraft.json`) in the web UI and start it.

## What to expect

- STARTING until Minecraft prints `Done (…)! For help, type "help"`, then
  RUNNING.
- **Stop** types `stop` into the console; Minecraft saves the world and
  exits. If it takes longer than 2 minutes, the session is killed.
- Console commands are typed into the Minecraft console.
- If the script refuses to start (EULA not accepted, jar missing, Java not
  found, already running), its message appears in the console and the server
  shows ERROR with the exit code.
- If Minecraft crashes, the server shows ERROR.
- Starting the script a second time by hand is refused while the server runs
  (a lock file protects the world).

Accept the EULA once before the first start: read
https://aka.ms/MinecraftEULA and set `eula=true` in `eula.txt` in the server
folder.
