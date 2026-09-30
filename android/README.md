# msnw for Android

A small app with two tabs: a browser that reaches services published with
`msnw export` (through the built-in HTTP proxy), and a terminal running real
`mosh` to a published host (`msnw mosh user@home`). msnw itself is unchanged:
the app bundles the Go binary plus `mosh-client`, dropbear's `dbclient` (as
the ssh used for the mosh bootstrap) and a terminfo database, and runs them
in a pty inside a Termux-derived terminal view.

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
- A foreground service keeps `msnw client --http-proxy 127.0.0.1:8080`
  running with a partial wake lock; the browser WebView uses it via
  `ProxyController` (localhost bypassed). Names such as `http://mypc/` go
  through the tunnel, everything else directly.
- The terminal tab runs `msnw mosh -v -log ... user@home` in a pty. Extra keys
  (Esc, Tab, Ctrl, arrows) sit above the keyboard; Reconnect starts a new
  session. Password prompts from dbclient appear in the terminal.

Settings: link key, mosh target (`user@name`), home page, optional server.

## Licenses

`terminal-emulator` and `terminal-view` are copied from
[termux-app](https://github.com/termux/termux-app) (GPLv3), so this app is
GPLv3 as well. mosh is GPLv3, dropbear MIT, OpenSSL Apache-2.0, protobuf
BSD-3, ncurses MIT-style.
