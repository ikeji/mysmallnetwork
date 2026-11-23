#!/bin/bash
# android-test.sh — run Android instrumented integration tests on a connected
# device or a headless KVM emulator (AVD default: "msnw").
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
SDK=${ANDROID_SDK_ROOT:-${ANDROID_HOME:-$HOME/Android/Sdk}}
ADB="$SDK/platform-tools/adb"
EMULATOR="$SDK/emulator/emulator"
AVD=${AVD:-msnw}

EMU_PID=""
cleanup() {
	if [ -n "$EMU_PID" ] && kill -0 "$EMU_PID" 2>/dev/null; then
		"$ADB" emu kill >/dev/null 2>&1 || true
		kill "$EMU_PID" >/dev/null 2>&1 || true
		wait "$EMU_PID" 2>/dev/null || true
	fi
}
trap cleanup EXIT

if ! "$ADB" devices | awk 'NR>1 && $2=="device" {found=1} END {exit !found}'; then
	echo "==> Starting headless emulator ($AVD)..."
	"$EMULATOR" -avd "$AVD" -no-window -no-audio -no-boot-anim -no-snapshot-save >/tmp/msnw-emulator.log 2>&1 &
	EMU_PID=$!
	"$ADB" wait-for-device
	for _ in $(seq 1 120); do
		if [ "$("$ADB" shell getprop sys.boot_completed 2>/dev/null | tr -d '\r')" = "1" ]; then
			break
		fi
		sleep 1
	done
	# Dismiss keyguard / animations so UI & WebView tests run reliably.
	"$ADB" shell wm dismiss-keyguard >/dev/null 2>&1 || true
	"$ADB" shell input keyevent 82 >/dev/null 2>&1 || true
fi

cd "$ROOT/android"
./gradlew --no-daemon :app:connectedDebugAndroidTest
