# msnw for Android

A small app with two tabs: a browser that reaches services published with
`msnw export` (through the built-in HTTP proxy), and a terminal running real
`mosh` to a published host (`msnw mosh user@home`). msnw itself is unchanged:
the app bundles the Go binary plus `mosh-client`, dropbear's `dbclient` (as
the ssh used for the mosh bootstrap) and a terminfo database, and runs them
in a pty inside a Termux-derived terminal view.

Prebuilt: `msnw-android-arm64.apk` on the
[Releases](https://github.com/ikeji/mysmallnetwork/releases) page, signed with
the project's release key (updates install over each other; a locally built
debug APK has a different signature and must be uninstalled first).

## Build

```
make cross                      # bin/linux-arm64/msnw (the Go binary)
android/native/build.sh         # mosh-client, dbclient, ssh shim, terminfo -> /tmp/msnw-wt/native/out
make android                    # android/app/build/outputs/apk/debug/app-debug.apk
adb install -r android/app/build/outputs/apk/debug/app-debug.apk
```

`android/native/build.sh` needs the NDK (`sdkmanager --install "ndk;27.2.12479018"`),
cmake, perl and a host C++ compiler; it downloads nothing, so put the source
tarballs of protobuf 3.21.12, OpenSSL 3.3.2, ncurses 6.5, mosh 1.4.0 and
dropbear 2024.86 under `$WORK/src` (see the script header). Output goes to
`$MSNW_NATIVE_OUT` (default `/tmp/msnw-wt/native/out`), which Gradle copies
into the APK. arm64-v8a only for now.

## How it works

- The executables ship in the APK as `lib/arm64-v8a/lib*.so` so Android
  extracts them to a directory the app may execute from. On start the app
  makes symlinks `bin/{msnw,mosh-client,dbclient,ssh}` to them and puts `bin`
  on PATH, so `msnw mosh` finds `ssh` and `mosh-client` by name as on a PC.
- `ssh` is a tiny C shim that maps the OpenSSH options `msnw mosh` uses
  (`-o ProxyCommand=...`, `-i`, `-p`) onto dbclient's command line and execs
  dbclient. Unknown host keys are accepted on first use (`-y`) and kept in the
  app's private `home/.ssh/known_hosts`.
- A service owns everything long-lived: the
  `msnw client --http-proxy 127.0.0.1:8080` process for the browser and the
  terminal sessions. The activity binds to it and only attaches views, so
  mosh sessions survive the activity being destroyed (back key, swipe from
  recents). The service is in the foreground (notification, wake lock) only
  while a mosh session is alive; that keeps the process from being killed in
  the background. mosh has no detach/attach (a new mosh-client cannot resume
  a server's session), so if the process does die the sessions are gone.
- The browser WebView uses the proxy via `ProxyController` (localhost
  bypassed). Names such as `http://mypc/` go through the tunnel, everything
  else directly.
- Each terminal tab runs `msnw mosh -v -log ... user@home` in a pty. Extra keys
  (Esc, Tab, Ctrl, arrows) sit above the keyboard; Reconnect starts a new
  session in the same tab. Password prompts from dbclient appear in the
  terminal unless an ssh key is set up.

- msnw is the same static (CGO_ENABLED=0) binary as on other platforms. Its
  pure-Go resolver reads /etc/resolv.conf, which Android lacks, so the app
  resolves the rendezvous server with the system resolver and passes the IP
  in `MSNW_SERVER`. Nothing else in msnw needs DNS. (An Android-specific
  cgo build with GOOS=android would remove this step if it is ever needed.)

The app opens on the "+" screen (also reached from the tab strip): type a URL
or a mosh target (`user@name`) to open a tab, or pick one from the history
lists below the inputs (long-press an entry to forget it). Settings on the
same screen: link key, optional server, ssh key, and the msnw log tail.

**SSH key**: "Generate / show key" in Settings creates an ed25519 key pair with
the bundled `dropbearkey` (kept in the app's private `home/.ssh/id_msnw`) and
shows the public key; add that line to `~/.ssh/authorized_keys` on the
exporter host. Once the key exists, the mosh bootstrap logs in with it
(`msnw mosh -ssh "-i ..."`); without it dbclient asks for the password in the
terminal.

## Licenses

`terminal-emulator` and `terminal-view` are copied from
[termux-app](https://github.com/termux/termux-app) (GPLv3), so this app is
GPLv3 as well. mosh is GPLv3, dropbear MIT, OpenSSL Apache-2.0, protobuf
BSD-3, ncurses MIT-style.
